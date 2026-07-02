package telemetry

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/bojanrajkovic/runny/internal/obs"
	"github.com/bojanrajkovic/runny/internal/traceid"
)

var (
	testCycle = obs.CycleRef{
		InstancePrefix: "host-abcd1234",
		Slot:           "pool-0",
		CycleID:        "a1b2c3d4",
		Started:        time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC),
	}
	testTraceID = traceid.Trace(testCycle.InstancePrefix, testCycle.Slot, testCycle.CycleID, testCycle.Started)
)

func newTestAssembler(t *testing.T) (obs.Emitter, *tracetest.InMemoryExporter) {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exp),
		sdktrace.WithIDGenerator(idGenerator{}),
	)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return NewTraceConsumer(tp.Tracer("test")), exp
}

func at(seconds int) time.Time { return testCycle.Started.Add(time.Duration(seconds) * time.Second) }

// TestTraceConsumerGolden walks a representative successful-cycle event
// stream through the consumer and checks the resulting span tree: names,
// parentage, deterministic IDs, attributes, and status — the golden test
// the issue's "Done when" asks for, covering a step-level Detail, an
// in-action Detail, VM info, an audit event, and JobEnded's deliberate drop.
func TestTraceConsumerGolden(t *testing.T) {
	emit, exp := newTestAssembler(t)

	var seq uint64
	next := func() uint64 { seq++; return seq }

	cycleStartSeq := next()
	emit(obs.Event{Seq: cycleStartSeq, Time: at(0), Cycle: testCycle, Kind: obs.KindCycleStarted})

	bootSeq := next()
	emit(obs.Event{
		Seq: bootSeq, Time: at(1), Cycle: testCycle, Step: "BOOT", Kind: obs.KindStepEntered,
		StepInfo: &obs.StepEvent{State: "BOOT"},
	})

	dialSeq := next()
	emit(obs.Event{
		Seq: dialSeq, Time: at(2), Cycle: testCycle, Step: "BOOT", Kind: obs.KindActionStarted,
		Action: &obs.ActionEvent{Name: "dial"},
	})

	emit(obs.Event{
		Seq: next(), Time: at(3), Cycle: testCycle, Step: "BOOT", Kind: obs.KindDetail,
		Detail: &obs.DetailEvent{Text: "dialing 10.0.0.5:22"},
	})

	emit(obs.Event{
		Seq: next(), Time: at(4), Cycle: testCycle, Step: "BOOT", Kind: obs.KindActionEnded,
		Action: &obs.ActionEvent{Name: "dial", Outcome: obs.OutcomeOK, Duration: 2 * time.Second},
	})

	// A step-level Detail (no action open) — must land on the step span, not
	// the just-closed action span.
	emit(obs.Event{
		Seq: next(), Time: at(5), Cycle: testCycle, Step: "BOOT", Kind: obs.KindDetail,
		Detail: &obs.DetailEvent{Text: "booted"},
	})

	emit(obs.Event{
		Seq: next(), Time: at(6), Cycle: testCycle, Step: "BOOT", Kind: obs.KindVMInfo,
		VM: &obs.VMEvent{MAC: "aa:bb:cc:dd:ee:ff"},
	})
	emit(obs.Event{
		Seq: next(), Time: at(7), Cycle: testCycle, Step: "BOOT", Kind: obs.KindVMInfo,
		VM: &obs.VMEvent{IP: "10.0.0.5"},
	})

	emit(obs.Event{
		Seq: next(), Time: at(8), Cycle: testCycle, Step: "BOOT", Kind: obs.KindStepLeft,
		StepInfo: &obs.StepEvent{State: "BOOT", Outcome: obs.OutcomeOK},
	})

	jobSeq := next()
	emit(obs.Event{
		Seq: jobSeq, Time: at(9), Cycle: testCycle, Step: "JOB", Kind: obs.KindStepEntered,
		StepInfo: &obs.StepEvent{State: "JOB"},
	})
	emit(obs.Event{
		Seq: next(), Time: at(10), Cycle: testCycle, Step: "JOB", Kind: obs.KindJobStarted,
		Job: &obs.JobEvent{Name: "build"},
	})
	emit(obs.Event{
		Seq: next(), Time: at(11), Cycle: testCycle, Step: "JOB", Kind: obs.KindStepLeft,
		StepInfo: &obs.StepEvent{State: "JOB", Outcome: obs.OutcomeOK},
	})
	// JobEnded arrives after StepLeft closed the JOB span — must not panic,
	// must not resurrect a span, must not appear anywhere in the output.
	emit(obs.Event{
		Seq: next(), Time: at(12), Cycle: testCycle, Step: "JOB", Kind: obs.KindJobEnded,
		Job: &obs.JobEvent{Name: "build", Outcome: obs.OutcomeOK},
	})

	emit(obs.Event{
		Seq: next(), Time: at(13), Cycle: testCycle, Kind: obs.KindAuditAppend,
		Audit: &obs.AuditEvent{Fingerprint: "SHA256:testfp", Outcome: "pending", State: "JOB"},
	})

	emit(obs.Event{
		Seq: next(), Time: at(14), Cycle: testCycle, Kind: obs.KindCycleFinished,
		Finish: &obs.FinishEvent{Result: "success", Ending: "success"},
	})

	spans := exp.GetSpans()
	if len(spans) != 4 {
		t.Fatalf("got %d spans, want 4 (root, BOOT step, dial action, JOB step)", len(spans))
	}

	byName := map[string]tracetest.SpanStub{}
	for _, s := range spans {
		byName[s.Name] = s
	}

	root, ok := byName["runny.cycle"]
	if !ok {
		t.Fatal("missing root span runny.cycle")
	}
	wantRootTrace := trace.TraceID(testTraceID)
	if root.SpanContext.TraceID() != wantRootTrace {
		t.Errorf("root trace ID = %x, want %x", root.SpanContext.TraceID(), wantRootTrace)
	}
	wantRootSpan := trace.SpanID(traceid.Span(testTraceID, "cycle", "", ""))
	if root.SpanContext.SpanID() != wantRootSpan {
		t.Errorf("root span ID = %x, want %x", root.SpanContext.SpanID(), wantRootSpan)
	}
	if root.StartTime.UTC() != at(0).UTC() || root.EndTime.UTC() != at(14).UTC() {
		t.Errorf("root span time = [%v, %v], want [%v, %v]", root.StartTime, root.EndTime, at(0), at(14))
	}
	if root.Status.Code == codes.Error {
		t.Errorf("root status = Error, want Unset/Ok for a successful ending")
	}

	boot, ok := byName["cycle.step BOOT"]
	if !ok {
		t.Fatal("missing step span cycle.step BOOT")
	}
	if boot.Parent.SpanID() != root.SpanContext.SpanID() {
		t.Errorf("BOOT step's parent span ID = %x, want root's %x", boot.Parent.SpanID(), root.SpanContext.SpanID())
	}
	wantBootSpan := trace.SpanID(traceid.Span(testTraceID, "step", "BOOT", ""))
	if boot.SpanContext.SpanID() != wantBootSpan {
		t.Errorf("BOOT span ID = %x, want %x", boot.SpanContext.SpanID(), wantBootSpan)
	}
	if got := attrString(boot.Attributes, "runny.progress.last"); got != "booted" {
		t.Errorf("BOOT progress.last = %q, want %q (the step-level Detail, not the in-action one)", got, "booted")
	}

	dial, ok := byName["cycle.step.action dial"]
	if !ok {
		t.Fatal("missing action span cycle.step.action dial")
	}
	if dial.Parent.SpanID() != boot.SpanContext.SpanID() {
		t.Errorf("dial action's parent span ID = %x, want BOOT's %x", dial.Parent.SpanID(), boot.SpanContext.SpanID())
	}
	wantDialSpan := trace.SpanID(traceid.Span(testTraceID, "action", "BOOT", "dial"))
	if dial.SpanContext.SpanID() != wantDialSpan {
		t.Errorf("dial span ID = %x, want %x", dial.SpanContext.SpanID(), wantDialSpan)
	}
	if got := attrString(dial.Attributes, "runny.progress.last"); got != "dialing 10.0.0.5:22" {
		t.Errorf("dial progress.last = %q, want the in-action Detail text", got)
	}

	if _, ok := byName["cycle.step JOB"]; !ok {
		t.Fatal("missing step span cycle.step JOB")
	}

	if got := attrString(root.Attributes, "vm.mac"); got != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("root vm.mac = %q", got)
	}
	if got := attrString(root.Attributes, "vm.ip"); got != "10.0.0.5" {
		t.Errorf("root vm.ip = %q", got)
	}
	if got := attrString(root.Attributes, "runny.ending"); got != "success" {
		t.Errorf("root runny.ending = %q, want success", got)
	}

	foundAudit := false
	for _, ev := range root.Events {
		if ev.Name == string(obs.KindAuditAppend) {
			foundAudit = true
		}
		if ev.Name == "job_ended" {
			t.Error("JobEnded must not appear as a root span event — it should be dropped entirely")
		}
	}
	if !foundAudit {
		t.Error("expected an audit_append span event on the root")
	}
}

func attrString(attrs []attribute.KeyValue, key string) string {
	for _, a := range attrs {
		if string(a.Key) == key {
			return a.Value.AsString()
		}
	}
	return ""
}

// TestTraceConsumerConcurrentDetail exercises the one documented exception
// to obs's single-goroutine-per-cycle contract: ENSURE_IMAGE progress
// events can arrive from the shared image puller's own goroutine while the
// cycle's FSM goroutine is concurrently entering/leaving other steps. Run
// under -race; this test's only job is to prove the per-cycle mutex makes
// that safe.
func TestTraceConsumerConcurrentDetail(t *testing.T) {
	emit, _ := newTestAssembler(t)

	emit(obs.Event{Seq: 1, Time: at(0), Cycle: testCycle, Kind: obs.KindCycleStarted})
	emit(obs.Event{
		Seq: 2, Time: at(1), Cycle: testCycle, Step: "ENSURE_IMAGE", Kind: obs.KindStepEntered,
		StepInfo: &obs.StepEvent{State: "ENSURE_IMAGE"},
	})

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			emit(obs.Event{
				Seq: uint64(100 + i), Time: at(1), Cycle: testCycle, Step: "ENSURE_IMAGE", Kind: obs.KindDetail,
				Detail: &obs.DetailEvent{Text: "pulling"},
			})
		}(i)
	}
	wg.Wait()

	emit(obs.Event{
		Seq: 3, Time: at(2), Cycle: testCycle, Step: "ENSURE_IMAGE", Kind: obs.KindStepLeft,
		StepInfo: &obs.StepEvent{State: "ENSURE_IMAGE", Outcome: obs.OutcomeOK},
	})
	emit(obs.Event{
		Seq: 4, Time: at(3), Cycle: testCycle, Kind: obs.KindCycleFinished,
		Finish: &obs.FinishEvent{Result: "success", Ending: "success"},
	})
}

// TestTraceConsumerDeadlineIsFailure regression-tests a bug caught in review:
// stepLeft/actionEnded only checked obs.OutcomeError, so a state that missed
// its bounded.Context deadline (cycle.OutcomeDeadline, wire value
// "deadline") rendered as a healthy span — the exact failure mode
// bounded.Context exists to catch, silently hidden from the trace.
func TestTraceConsumerDeadlineIsFailure(t *testing.T) {
	emit, exp := newTestAssembler(t)

	emit(obs.Event{Seq: 1, Time: at(0), Cycle: testCycle, Kind: obs.KindCycleStarted})
	emit(obs.Event{
		Seq: 2, Time: at(1), Cycle: testCycle, Step: "BOOT", Kind: obs.KindStepEntered,
		StepInfo: &obs.StepEvent{State: "BOOT"},
	})
	emit(obs.Event{
		Seq: 3, Time: at(2), Cycle: testCycle, Step: "BOOT", Kind: obs.KindStepLeft,
		StepInfo: &obs.StepEvent{State: "BOOT", Outcome: "deadline", Error: "context deadline exceeded"},
	})
	emit(obs.Event{
		Seq: 4, Time: at(3), Cycle: testCycle, Kind: obs.KindCycleFinished,
		Finish: &obs.FinishEvent{Result: "failure", Ending: "failure", FailureState: "BOOT", Error: "context deadline exceeded"},
	})

	for _, s := range exp.GetSpans() {
		if s.Name != "cycle.step BOOT" {
			continue
		}
		if s.Status.Code != codes.Error {
			t.Errorf("BOOT step status = %v, want Error for a deadline outcome", s.Status.Code)
		}
		return
	}
	t.Fatal("missing step span cycle.step BOOT")
}
