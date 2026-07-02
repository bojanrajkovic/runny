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
		Pool:           "macos-arm",
		CycleID:        "a1b2c3d4",
		RunnerName:     "host-abcd1234-pool-0-a1b2c3d4",
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
// parentage, deterministic IDs, attributes, and status — covering a
// step-level Detail, an in-action Detail, action attrs, VM and runner info,
// the job's operator-key audit, and a fully-populated audit event.
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
		Action: &obs.ActionEvent{Name: "dial", Attrs: []obs.Attr{{Key: "runny.hardening", Value: "scramble"}}},
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
		Seq: next(), Time: at(7), Cycle: testCycle, Step: "BOOT", Kind: obs.KindRunnerInfo,
		Runner: &obs.RunnerEvent{ID: 424242},
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
	// JobEnded fires before StepLeft (the FSM brackets job events inside the
	// step) and carries the job's operator-key audit.
	emit(obs.Event{
		Seq: next(), Time: at(11), Cycle: testCycle, Step: "JOB", Kind: obs.KindJobEnded,
		Job: &obs.JobEvent{Name: "build", Outcome: obs.OutcomeOK, OperatorKeys: []string{"SHA256:testfp"}},
	})
	emit(obs.Event{
		Seq: next(), Time: at(12), Cycle: testCycle, Step: "JOB", Kind: obs.KindStepLeft,
		StepInfo: &obs.StepEvent{State: "JOB", Outcome: obs.OutcomeOK},
	})

	opUID := uint32(501)
	emit(obs.Event{
		Seq: next(), Time: at(13), Cycle: testCycle, Kind: obs.KindAuditAppend,
		Audit: &obs.AuditEvent{
			Fingerprint: "SHA256:testfp", Comment: "oncall laptop", Reason: "flaky dns",
			Outcome: "pending", State: "JOB", OperatorUID: &opUID, OperatorUser: "brajkovic",
		},
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
	if got := attrInt64(boot.Attributes, "runny.runner.id"); got != 424242 {
		t.Errorf("BOOT runny.runner.id = %d, want 424242 (from the RunnerInfo event)", got)
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
	if got := attrString(dial.Attributes, "runny.hardening"); got != "scramble" {
		t.Errorf("dial runny.hardening = %q, want the action attr passed through", got)
	}

	if _, ok := byName["cycle.step JOB"]; !ok {
		t.Fatal("missing step span cycle.step JOB")
	}
	if got := attr(root.Attributes, "runny.job.operator_keys").AsStringSlice(); len(got) != 1 || got[0] != "SHA256:testfp" {
		t.Errorf("root runny.job.operator_keys = %v, want [SHA256:testfp]", got)
	}

	if got := attrString(root.Attributes, "runny.pool"); got != "macos-arm" {
		t.Errorf("root runny.pool = %q", got)
	}
	if got := attrString(root.Attributes, "runny.runner_name"); got != testCycle.RunnerName {
		t.Errorf("root runny.runner_name = %q", got)
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
		if ev.Name != string(obs.KindAuditAppend) {
			continue
		}
		foundAudit = true
		if got := attrString(ev.Attributes, "runny.audit.comment"); got != "oncall laptop" {
			t.Errorf("audit comment = %q", got)
		}
		if got := attrString(ev.Attributes, "runny.audit.reason"); got != "flaky dns" {
			t.Errorf("audit reason = %q", got)
		}
		if got := attrInt64(ev.Attributes, "runny.audit.operator_uid"); got != 501 {
			t.Errorf("audit operator_uid = %d, want 501", got)
		}
		if got := attrString(ev.Attributes, "runny.audit.operator_user"); got != "brajkovic" {
			t.Errorf("audit operator_user = %q", got)
		}
	}
	if !foundAudit {
		t.Error("expected an audit_append span event on the root")
	}
}

// attr returns the value for key (zero Value when absent — AsString gives
// "", AsInt64 gives 0, AsStringSlice gives nil).
func attr(attrs []attribute.KeyValue, key string) attribute.Value {
	set := attribute.NewSet(attrs...)
	v, _ := set.Value(attribute.Key(key))
	return v
}

func attrString(attrs []attribute.KeyValue, key string) string { return attr(attrs, key).AsString() }
func attrInt64(attrs []attribute.KeyValue, key string) int64   { return attr(attrs, key).AsInt64() }

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

// TestTraceConsumerLateJobEndedStillLandsOnRoot pins the ordering immunity
// jobEnded gets from attaching to the root: a journal replayed from a
// daemon that emitted JobEnded after StepLeft (the pre-reorder shape) must
// still record the operator keys, not silently drop them, and must not
// panic or resurrect the closed step span.
func TestTraceConsumerLateJobEndedStillLandsOnRoot(t *testing.T) {
	emit, exp := newTestAssembler(t)

	emit(obs.Event{Seq: 1, Time: at(0), Cycle: testCycle, Kind: obs.KindCycleStarted})
	emit(obs.Event{
		Seq: 2, Time: at(1), Cycle: testCycle, Step: "JOB", Kind: obs.KindStepEntered,
		StepInfo: &obs.StepEvent{State: "JOB"},
	})
	emit(obs.Event{
		Seq: 3, Time: at(2), Cycle: testCycle, Step: "JOB", Kind: obs.KindStepLeft,
		StepInfo: &obs.StepEvent{State: "JOB", Outcome: obs.OutcomeOK},
	})
	// The old order: JobEnded after its step closed.
	emit(obs.Event{
		Seq: 4, Time: at(3), Cycle: testCycle, Step: "JOB", Kind: obs.KindJobEnded,
		Job: &obs.JobEvent{Name: "build", Outcome: obs.OutcomeOK, OperatorKeys: []string{"SHA256:latefp"}},
	})
	emit(obs.Event{
		Seq: 5, Time: at(4), Cycle: testCycle, Kind: obs.KindCycleFinished,
		Finish: &obs.FinishEvent{Result: "success", Ending: "success"},
	})

	spans := exp.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2 (root, JOB step) — a late JobEnded must not mint a span", len(spans))
	}
	for _, s := range spans {
		if s.Name != "runny.cycle" {
			continue
		}
		if got := attr(s.Attributes, "runny.job.operator_keys").AsStringSlice(); len(got) != 1 || got[0] != "SHA256:latefp" {
			t.Errorf("root runny.job.operator_keys = %v, want [SHA256:latefp] even when JobEnded arrives late", got)
		}
		return
	}
	t.Fatal("missing root span")
}

// TestTraceConsumerImageIdentity pins the image-identity lifting: the pool's
// configured ref rides CycleRef onto the root at start, and the image_info
// events (digest at resolve, runner version at tarball ensure) land on both
// the cycle root and the owning ENSURE_IMAGE step span — traces are
// queryable by image without joining against cycle.json.
func TestTraceConsumerImageIdentity(t *testing.T) {
	emit, exp := newTestAssembler(t)
	ref := testCycle
	ref.Image = "ghcr.io/test/image:1"

	emit(obs.Event{Seq: 1, Time: at(0), Cycle: ref, Kind: obs.KindCycleStarted})
	emit(obs.Event{
		Seq: 2, Time: at(1), Cycle: ref, Step: "ENSURE_IMAGE", Kind: obs.KindStepEntered,
		StepInfo: &obs.StepEvent{State: "ENSURE_IMAGE"},
	})
	emit(obs.Event{
		Seq: 3, Time: at(2), Cycle: ref, Step: "ENSURE_IMAGE", Kind: obs.KindImageInfo,
		Image: &obs.ImageEvent{Digest: "sha256:abc"},
	})
	emit(obs.Event{
		Seq: 4, Time: at(3), Cycle: ref, Step: "ENSURE_IMAGE", Kind: obs.KindImageInfo,
		Image: &obs.ImageEvent{RunnerVersion: "actions-runner-osx-arm64-2.320.0.tar.gz"},
	})
	emit(obs.Event{
		Seq: 5, Time: at(4), Cycle: ref, Step: "ENSURE_IMAGE", Kind: obs.KindStepLeft,
		StepInfo: &obs.StepEvent{State: "ENSURE_IMAGE", Outcome: obs.OutcomeOK},
	})
	emit(obs.Event{
		Seq: 6, Time: at(5), Cycle: ref, Kind: obs.KindCycleFinished,
		Finish: &obs.FinishEvent{Result: "success", Ending: "success"},
	})

	byName := map[string]tracetest.SpanStub{}
	for _, s := range exp.GetSpans() {
		byName[s.Name] = s
	}
	root, ok := byName["runny.cycle"]
	if !ok {
		t.Fatal("missing root span runny.cycle")
	}
	if got := attrString(root.Attributes, "runny.image.ref"); got != "ghcr.io/test/image:1" {
		t.Errorf("root runny.image.ref = %q, want the configured ref", got)
	}
	if got := attrString(root.Attributes, "runny.image.digest"); got != "sha256:abc" {
		t.Errorf("root runny.image.digest = %q", got)
	}
	if got := attrString(root.Attributes, "runny.runner_version"); got != "actions-runner-osx-arm64-2.320.0.tar.gz" {
		t.Errorf("root runny.runner_version = %q", got)
	}

	step, ok := byName["cycle.step ENSURE_IMAGE"]
	if !ok {
		t.Fatal("missing step span cycle.step ENSURE_IMAGE")
	}
	if got := attrString(step.Attributes, "runny.image.digest"); got != "sha256:abc" {
		t.Errorf("step runny.image.digest = %q", got)
	}
	if got := attrString(step.Attributes, "runny.runner_version"); got != "actions-runner-osx-arm64-2.320.0.tar.gz" {
		t.Errorf("step runny.runner_version = %q", got)
	}
}

// TestTraceConsumerHTTPSpans covers the KindHTTP fold: an event during an
// open action parents under that action's span; one with no action open
// parents under the step; repeats of the same class get distinct
// seq-derived span IDs; timing reconstructs start as Time − Duration; and a
// transport-level failure (Status 0 + Error) sets span error status.
func TestTraceConsumerHTTPSpans(t *testing.T) {
	emit, exp := newTestAssembler(t)

	emit(obs.Event{Seq: 1, Time: at(0), Cycle: testCycle, Kind: obs.KindCycleStarted})
	emit(obs.Event{
		Seq: 2, Time: at(1), Cycle: testCycle, Step: "ENSURE_IMAGE", Kind: obs.KindStepEntered,
		StepInfo: &obs.StepEvent{State: "ENSURE_IMAGE"},
	})
	emit(obs.Event{
		Seq: 3, Time: at(2), Cycle: testCycle, Step: "ENSURE_IMAGE", Kind: obs.KindActionStarted,
		Action: &obs.ActionEvent{Name: "resolve"},
	})
	// Two manifest round trips inside the resolve action: a 401 challenge
	// answered by the token dance, then the authenticated retry.
	emit(obs.Event{
		Seq: 4, Time: at(3), Cycle: testCycle, Step: "ENSURE_IMAGE", Kind: obs.KindHTTP,
		HTTP: &obs.HTTPEvent{Class: obs.HTTPRegistryManifest, Method: "GET", Host: "ghcr.io", Status: 401, Duration: time.Second},
	})
	emit(obs.Event{
		Seq: 5, Time: at(5), Cycle: testCycle, Step: "ENSURE_IMAGE", Kind: obs.KindHTTP,
		HTTP: &obs.HTTPEvent{Class: obs.HTTPRegistryManifest, Method: "GET", Host: "ghcr.io", Status: 200, Duration: time.Second},
	})
	emit(obs.Event{
		Seq: 6, Time: at(6), Cycle: testCycle, Step: "ENSURE_IMAGE", Kind: obs.KindActionEnded,
		Action: &obs.ActionEvent{Name: "resolve", Outcome: obs.OutcomeOK, Duration: 4 * time.Second},
	})
	emit(obs.Event{
		Seq: 7, Time: at(7), Cycle: testCycle, Step: "ENSURE_IMAGE", Kind: obs.KindStepLeft,
		StepInfo: &obs.StepEvent{State: "ENSURE_IMAGE", Outcome: obs.OutcomeOK},
	})
	// MINT_JIT has no actions: its round trips parent under the step span,
	// and a transport-level failure carries error status.
	emit(obs.Event{
		Seq: 8, Time: at(8), Cycle: testCycle, Step: "MINT_JIT", Kind: obs.KindStepEntered,
		StepInfo: &obs.StepEvent{State: "MINT_JIT"},
	})
	emit(obs.Event{
		Seq: 9, Time: at(10), Cycle: testCycle, Step: "MINT_JIT", Kind: obs.KindHTTP,
		HTTP: &obs.HTTPEvent{Class: obs.HTTPGitHubJIT, Method: "POST", Host: "api.github.com", Error: "context deadline exceeded", Duration: 2 * time.Second},
	})
	emit(obs.Event{
		Seq: 10, Time: at(11), Cycle: testCycle, Step: "MINT_JIT", Kind: obs.KindStepLeft,
		StepInfo: &obs.StepEvent{State: "MINT_JIT", Outcome: obs.OutcomeError, Error: "mint failed"},
	})
	emit(obs.Event{
		Seq: 11, Time: at(12), Cycle: testCycle, Kind: obs.KindCycleFinished,
		Finish: &obs.FinishEvent{Result: "failure", Ending: "failure"},
	})

	var manifests []tracetest.SpanStub
	var jit *tracetest.SpanStub
	for _, s := range exp.GetSpans() {
		switch s.Name {
		case "http registry.manifest":
			manifests = append(manifests, s)
		case "http github.jit":
			jit = &s
		}
	}
	if len(manifests) != 2 {
		t.Fatalf("got %d registry.manifest spans, want 2", len(manifests))
	}
	if jit == nil {
		t.Fatal("missing http github.jit span")
	}

	actionSID := traceid.Span(testTraceID, "action", "ENSURE_IMAGE", "resolve")
	stepSID := traceid.Span(testTraceID, "step", "MINT_JIT", "")
	for i, m := range manifests {
		if got := m.Parent.SpanID(); got != trace.SpanID(actionSID) {
			t.Errorf("manifest[%d] parent = %s, want the resolve action span", i, got)
		}
		if got := attrString(m.Attributes, "server.address"); got != "ghcr.io" {
			t.Errorf("manifest[%d] server.address = %q", i, got)
		}
		if got := attrString(m.Attributes, "http.request.method"); got != "GET" {
			t.Errorf("manifest[%d] method = %q", i, got)
		}
		if got := attrInt64(m.Attributes, "http.response.status_code"); got != [2]int64{401, 200}[i] {
			t.Errorf("manifest[%d] status_code = %d", i, got)
		}
	}
	if manifests[0].SpanContext.SpanID() == manifests[1].SpanContext.SpanID() {
		t.Error("repeated round trips share a span ID; seq must uniquify")
	}
	if got, want := manifests[1].StartTime, at(4); !got.Equal(want) {
		t.Errorf("manifest[1] start = %v, want Time − Duration = %v", got, want)
	}
	if got, want := manifests[1].EndTime, at(5); !got.Equal(want) {
		t.Errorf("manifest[1] end = %v, want %v", got, want)
	}
	// Semconv client rule: 4xx is span error even when the dance recovers —
	// the enclosing action's green status is what says recovery happened.
	if s := manifests[0].Status; s.Code != codes.Error || s.Description != "HTTP 401" {
		t.Errorf("401 challenge status = %+v, want Error \"HTTP 401\"", s)
	}
	if s := manifests[1].Status; s.Code == codes.Error {
		t.Error("the authenticated 200 retry must not carry error status")
	}

	if got := jit.Parent.SpanID(); got != trace.SpanID(stepSID) {
		t.Errorf("jit parent = %s, want the MINT_JIT step span", got)
	}
	if jit.Status.Code != codes.Error || jit.Status.Description != "context deadline exceeded" {
		t.Errorf("jit status = %+v, want transport error", jit.Status)
	}
	if got := attrInt64(jit.Attributes, "http.response.status_code"); got != 0 {
		t.Errorf("jit carries status_code %d, want absent on transport error", got)
	}
}
