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

	"github.com/bojanrajkovic/runny/internal/obs"
)

var testCycle = obs.CycleRef{
	Slot:       "pool-0",
	Pool:       "macos-arm",
	CycleID:    "a1b2c3d4",
	RunnerName: "host-abcd1234-pool-0-a1b2c3d4",
	Started:    time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC),
}

func newTestAssembler(t *testing.T) (obs.Emitter, *tracetest.InMemoryExporter) {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return NewTraceConsumer(tp.Tracer("test")), exp
}

func at(seconds int) time.Time { return testCycle.Started.Add(time.Duration(seconds) * time.Second) }

// TestTraceConsumerGolden walks a representative successful-cycle event
// stream through the consumer and checks the resulting span tree: names,
// parentage, attributes, and status — covering a step-level Detail, an
// in-action Detail, action attrs, VM and runner info, the job's operator-key
// audit, and a fully-populated audit event.
func TestTraceConsumerGolden(t *testing.T) {
	emit, exp := newTestAssembler(t)

	emit(obs.Event{Time: at(0), Cycle: testCycle, Kind: obs.KindCycleStarted})

	emit(obs.Event{
		Time: at(1), Cycle: testCycle, Step: "BOOT", Kind: obs.KindStepEntered,
		StepInfo: &obs.StepEvent{},
	})

	emit(obs.Event{
		Time: at(2), Cycle: testCycle, Step: "BOOT", Kind: obs.KindActionStarted,
		Action: &obs.ActionEvent{Name: "dial", Attrs: []obs.Attr{{Key: "runny.hardening", Value: "scramble"}}},
	})

	emit(obs.Event{
		Time: at(3), Cycle: testCycle, Step: "BOOT", Kind: obs.KindDetail,
		Detail: &obs.DetailEvent{Text: "dialing 10.0.0.5:22"},
	})

	emit(obs.Event{
		Time: at(4), Cycle: testCycle, Step: "BOOT", Kind: obs.KindActionEnded,
		Action: &obs.ActionEvent{Name: "dial", Outcome: obs.OutcomeOK, Duration: 2 * time.Second},
	})

	// A step-level Detail (no action open) — must land on the step span, not
	// the just-closed action span.
	emit(obs.Event{
		Time: at(5), Cycle: testCycle, Step: "BOOT", Kind: obs.KindDetail,
		Detail: &obs.DetailEvent{Text: "booted"},
	})

	emit(obs.Event{
		Time: at(6), Cycle: testCycle, Step: "BOOT", Kind: obs.KindVMInfo,
		VM: &obs.VMEvent{MAC: "aa:bb:cc:dd:ee:ff"},
	})
	emit(obs.Event{
		Time: at(7), Cycle: testCycle, Step: "BOOT", Kind: obs.KindVMInfo,
		VM: &obs.VMEvent{IP: "10.0.0.5"},
	})
	emit(obs.Event{
		Time: at(7), Cycle: testCycle, Step: "BOOT", Kind: obs.KindRunnerInfo,
		Runner: &obs.RunnerEvent{ID: 424242},
	})

	emit(obs.Event{
		Time: at(8), Cycle: testCycle, Step: "BOOT", Kind: obs.KindStepLeft,
		StepInfo: &obs.StepEvent{Outcome: obs.OutcomeOK},
	})

	emit(obs.Event{
		Time: at(9), Cycle: testCycle, Step: "JOB", Kind: obs.KindStepEntered,
		StepInfo: &obs.StepEvent{},
	})
	emit(obs.Event{
		Time: at(10), Cycle: testCycle, Step: "JOB", Kind: obs.KindJobStarted,
		Job: &obs.JobEvent{Name: "build"},
	})
	// JobEnded fires before StepLeft (the FSM brackets job events inside the
	// step) and carries the job's operator-key audit.
	emit(obs.Event{
		Time: at(11), Cycle: testCycle, Step: "JOB", Kind: obs.KindJobEnded,
		Job: &obs.JobEvent{Name: "build", Outcome: obs.OutcomeOK, OperatorKeys: []string{"SHA256:testfp"}},
	})
	emit(obs.Event{
		Time: at(12), Cycle: testCycle, Step: "JOB", Kind: obs.KindStepLeft,
		StepInfo: &obs.StepEvent{Outcome: obs.OutcomeOK},
	})

	opUID := uint32(501)
	emit(obs.Event{
		Time: at(13), Cycle: testCycle, Kind: obs.KindAuditAppend,
		Audit: &obs.AuditEvent{
			Fingerprint: "SHA256:testfp", Comment: "oncall laptop", Reason: "flaky dns",
			Outcome: "pending", State: "JOB", OperatorUID: &opUID, OperatorUser: "brajkovic",
		},
	})

	emit(obs.Event{
		Time: at(14), Cycle: testCycle, Kind: obs.KindCycleFinished,
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
	if !root.SpanContext.TraceID().IsValid() || !root.SpanContext.SpanID().IsValid() {
		t.Errorf("root span carries an invalid (SDK-random) trace/span ID: %+v", root.SpanContext)
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
	if boot.SpanContext.TraceID() != root.SpanContext.TraceID() {
		t.Errorf("BOOT trace ID = %x, want root's trace %x", boot.SpanContext.TraceID(), root.SpanContext.TraceID())
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
	if dial.SpanContext.TraceID() != root.SpanContext.TraceID() {
		t.Errorf("dial trace ID = %x, want root's trace %x", dial.SpanContext.TraceID(), root.SpanContext.TraceID())
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

	emit(obs.Event{Time: at(0), Cycle: testCycle, Kind: obs.KindCycleStarted})
	emit(obs.Event{
		Time: at(1), Cycle: testCycle, Step: "ENSURE_IMAGE", Kind: obs.KindStepEntered,
		StepInfo: &obs.StepEvent{},
	})

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			emit(obs.Event{
				Time: at(1), Cycle: testCycle, Step: "ENSURE_IMAGE", Kind: obs.KindDetail,
				Detail: &obs.DetailEvent{Text: "pulling"},
			})
		}(i)
	}
	wg.Wait()

	emit(obs.Event{
		Time: at(2), Cycle: testCycle, Step: "ENSURE_IMAGE", Kind: obs.KindStepLeft,
		StepInfo: &obs.StepEvent{Outcome: obs.OutcomeOK},
	})
	emit(obs.Event{
		Time: at(3), Cycle: testCycle, Kind: obs.KindCycleFinished,
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

	emit(obs.Event{Time: at(0), Cycle: testCycle, Kind: obs.KindCycleStarted})
	emit(obs.Event{
		Time: at(1), Cycle: testCycle, Step: "BOOT", Kind: obs.KindStepEntered,
		StepInfo: &obs.StepEvent{},
	})
	emit(obs.Event{
		Time: at(2), Cycle: testCycle, Step: "BOOT", Kind: obs.KindStepLeft,
		StepInfo: &obs.StepEvent{Outcome: "deadline", Error: "context deadline exceeded"},
	})
	emit(obs.Event{
		Time: at(3), Cycle: testCycle, Kind: obs.KindCycleFinished,
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

	emit(obs.Event{Time: at(0), Cycle: testCycle, Kind: obs.KindCycleStarted})
	emit(obs.Event{
		Time: at(1), Cycle: testCycle, Step: "JOB", Kind: obs.KindStepEntered,
		StepInfo: &obs.StepEvent{},
	})
	emit(obs.Event{
		Time: at(2), Cycle: testCycle, Step: "JOB", Kind: obs.KindStepLeft,
		StepInfo: &obs.StepEvent{Outcome: obs.OutcomeOK},
	})
	// The old order: JobEnded after its step closed.
	emit(obs.Event{
		Time: at(3), Cycle: testCycle, Step: "JOB", Kind: obs.KindJobEnded,
		Job: &obs.JobEvent{Name: "build", Outcome: obs.OutcomeOK, OperatorKeys: []string{"SHA256:latefp"}},
	})
	emit(obs.Event{
		Time: at(4), Cycle: testCycle, Kind: obs.KindCycleFinished,
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

	emit(obs.Event{Time: at(0), Cycle: ref, Kind: obs.KindCycleStarted})
	emit(obs.Event{
		Time: at(1), Cycle: ref, Step: "ENSURE_IMAGE", Kind: obs.KindStepEntered,
		StepInfo: &obs.StepEvent{},
	})
	emit(obs.Event{
		Time: at(2), Cycle: ref, Step: "ENSURE_IMAGE", Kind: obs.KindImageInfo,
		Image: &obs.ImageEvent{Digest: "sha256:abc"},
	})
	emit(obs.Event{
		Time: at(3), Cycle: ref, Step: "ENSURE_IMAGE", Kind: obs.KindImageInfo,
		Image: &obs.ImageEvent{RunnerVersion: "actions-runner-osx-arm64-2.320.0.tar.gz"},
	})
	emit(obs.Event{
		Time: at(4), Cycle: ref, Step: "ENSURE_IMAGE", Kind: obs.KindStepLeft,
		StepInfo: &obs.StepEvent{Outcome: obs.OutcomeOK},
	})
	emit(obs.Event{
		Time: at(5), Cycle: ref, Kind: obs.KindCycleFinished,
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
// (SDK-random) span IDs; timing reconstructs start as Time − Duration; and a
// transport-level failure (Status 0 + Error) sets span error status.
func TestTraceConsumerHTTPSpans(t *testing.T) {
	emit, exp := newTestAssembler(t)

	emit(obs.Event{Time: at(0), Cycle: testCycle, Kind: obs.KindCycleStarted})
	emit(obs.Event{
		Time: at(1), Cycle: testCycle, Step: "ENSURE_IMAGE", Kind: obs.KindStepEntered,
		StepInfo: &obs.StepEvent{},
	})
	emit(obs.Event{
		Time: at(2), Cycle: testCycle, Step: "ENSURE_IMAGE", Kind: obs.KindActionStarted,
		Action: &obs.ActionEvent{Name: "resolve"},
	})
	// Two manifest round trips inside the resolve action: a 401 challenge
	// answered by the token dance, then the authenticated retry.
	emit(obs.Event{
		Time: at(3), Cycle: testCycle, Step: "ENSURE_IMAGE", Kind: obs.KindHTTP,
		HTTP: &obs.HTTPEvent{Class: obs.HTTPRegistryManifest, Method: "GET", Host: "ghcr.io", Status: 401, Duration: time.Second},
	})
	emit(obs.Event{
		Time: at(5), Cycle: testCycle, Step: "ENSURE_IMAGE", Kind: obs.KindHTTP,
		HTTP: &obs.HTTPEvent{
			Class: obs.HTTPRegistryManifest, Method: "GET", Host: "ghcr.io", Status: 200,
			Duration: time.Second, HeaderDuration: 200 * time.Millisecond, BytesRead: 2048,
		},
	})
	emit(obs.Event{
		Time: at(6), Cycle: testCycle, Step: "ENSURE_IMAGE", Kind: obs.KindActionEnded,
		Action: &obs.ActionEvent{Name: "resolve", Outcome: obs.OutcomeOK, Duration: 4 * time.Second},
	})
	emit(obs.Event{
		Time: at(7), Cycle: testCycle, Step: "ENSURE_IMAGE", Kind: obs.KindStepLeft,
		StepInfo: &obs.StepEvent{Outcome: obs.OutcomeOK},
	})
	// MINT_JIT has no actions: its round trips parent under the step span,
	// and a transport-level failure carries error status.
	emit(obs.Event{
		Time: at(8), Cycle: testCycle, Step: "MINT_JIT", Kind: obs.KindStepEntered,
		StepInfo: &obs.StepEvent{},
	})
	emit(obs.Event{
		Time: at(10), Cycle: testCycle, Step: "MINT_JIT", Kind: obs.KindHTTP,
		HTTP: &obs.HTTPEvent{Class: obs.HTTPGitHubJIT, Method: "POST", Host: "api.github.com", Error: "context deadline exceeded", Duration: 2 * time.Second},
	})
	emit(obs.Event{
		Time: at(11), Cycle: testCycle, Step: "MINT_JIT", Kind: obs.KindStepLeft,
		StepInfo: &obs.StepEvent{Outcome: obs.OutcomeError, Error: "mint failed"},
	})
	emit(obs.Event{
		Time: at(12), Cycle: testCycle, Kind: obs.KindCycleFinished,
		Finish: &obs.FinishEvent{Result: "failure", Ending: "failure"},
	})

	var manifests []tracetest.SpanStub
	var jit *tracetest.SpanStub
	var resolveAction, mintJitStep *tracetest.SpanStub
	for _, s := range exp.GetSpans() {
		switch s.Name {
		case "http registry.manifest":
			manifests = append(manifests, s)
		case "http github.jit":
			jit = &s
		case "cycle.step.action resolve":
			resolveAction = &s
		case "cycle.step MINT_JIT":
			mintJitStep = &s
		}
	}
	if len(manifests) != 2 {
		t.Fatalf("got %d registry.manifest spans, want 2", len(manifests))
	}
	if jit == nil {
		t.Fatal("missing http github.jit span")
	}
	if resolveAction == nil {
		t.Fatal("missing action span cycle.step.action resolve")
	}
	if mintJitStep == nil {
		t.Fatal("missing step span cycle.step MINT_JIT")
	}

	for i, m := range manifests {
		if got := m.Parent.SpanID(); got != resolveAction.SpanContext.SpanID() {
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
		t.Error("repeated round trips share a span ID; the SDK's random generator must not collide within one test run")
	}
	if got, want := manifests[1].StartTime, at(4); !got.Equal(want) {
		t.Errorf("manifest[1] start = %v, want Time − Duration = %v", got, want)
	}
	if got, want := manifests[1].EndTime, at(5); !got.Equal(want) {
		t.Errorf("manifest[1] end = %v, want %v", got, want)
	}
	if got := attrInt64(manifests[1].Attributes, "runny.http.bytes"); got != 2048 {
		t.Errorf("manifest[1] runny.http.bytes = %d, want 2048", got)
	}
	// The headers marker sits inside the span at start + HeaderDuration.
	var headerEvents int
	for _, ev := range manifests[1].Events {
		if ev.Name != "headers" {
			continue
		}
		headerEvents++
		if want := at(4).Add(200 * time.Millisecond); !ev.Time.Equal(want) {
			t.Errorf("headers event at %v, want %v", ev.Time, want)
		}
	}
	if headerEvents != 1 {
		t.Errorf("got %d headers events on manifest[1], want 1", headerEvents)
	}
	if len(jit.Events) != 0 {
		t.Errorf("transport-error span carries %d events, want none (headers never arrived)", len(jit.Events))
	}
	// Semconv client rule: 4xx is span error even when the dance recovers —
	// the enclosing action's green status is what says recovery happened.
	if s := manifests[0].Status; s.Code != codes.Error || s.Description != "HTTP 401" {
		t.Errorf("401 challenge status = %+v, want Error \"HTTP 401\"", s)
	}
	if s := manifests[1].Status; s.Code == codes.Error {
		t.Error("the authenticated 200 retry must not carry error status")
	}

	if got := jit.Parent.SpanID(); got != mintJitStep.SpanContext.SpanID() {
		t.Errorf("jit parent = %s, want the MINT_JIT step span", got)
	}
	if jit.Status.Code != codes.Error || jit.Status.Description != "context deadline exceeded" {
		t.Errorf("jit status = %+v, want transport error", jit.Status)
	}
	if got := attrInt64(jit.Attributes, "http.response.status_code"); got != 0 {
		t.Errorf("jit carries status_code %d, want absent on transport error", got)
	}
}

// A scripted pull with two HTTP round trips renders as one runny.pull root
// with two client children — outcome, bytes, and the progress detail landing
// on the root at KindPullFinished — and the pull id it carries matches the
// cycle-scoped wait-for-pull action that waited on it: the correlation the
// two subtrees share instead of span parentage.
func TestTraceConsumerPullSpans(t *testing.T) {
	emit, exp := newTestAssembler(t)

	pull := &obs.PullRef{ID: "pull-abc123", Ref: "ghcr.io/x@sha256:d34d", Digest: "sha256:d34d", Started: at(0)}

	// The cycle side: ENSURE_IMAGE's wait-for-pull action, carrying the same
	// pull id the pull side will carry on its root.
	emit(obs.Event{Time: at(0), Cycle: testCycle, Kind: obs.KindCycleStarted})
	emit(obs.Event{
		Time: at(0), Cycle: testCycle, Step: "ENSURE_IMAGE", Kind: obs.KindStepEntered,
		StepInfo: &obs.StepEvent{},
	})
	emit(obs.Event{
		Time: at(0), Cycle: testCycle, Step: "ENSURE_IMAGE", Kind: obs.KindActionStarted,
		Action: &obs.ActionEvent{Name: obs.ActionWaitForPull, Attrs: []obs.Attr{{Key: obs.AttrPullID, Value: pull.ID}}},
	})

	// The pull side: started, a token exchange, a progress tick, the blob
	// GET, finished.
	emit(obs.Event{Time: at(1), Pull: pull, Kind: obs.KindPullStarted})
	emit(obs.Event{Time: at(2), Pull: pull, Kind: obs.KindHTTP, HTTP: &obs.HTTPEvent{
		Class: obs.HTTPRegistryToken, Method: "GET", Host: "registry.example", Status: 200, Duration: time.Second,
	}})
	emit(obs.Event{Time: at(4), Pull: pull, Kind: obs.KindDetail, Detail: &obs.DetailEvent{Text: "pulled 1 GiB at 500 MiB/s"}})
	emit(obs.Event{Time: at(5), Pull: pull, Kind: obs.KindHTTP, HTTP: &obs.HTTPEvent{
		Class: obs.HTTPRegistryBlob, Method: "GET", Host: "registry.example", Status: 200,
		Duration: 3 * time.Second, BytesRead: 1 << 30,
	}})
	emit(obs.Event{Time: at(6), Pull: pull, Kind: obs.KindPullFinished, PullInfo: &obs.PullEvent{
		Outcome: obs.OutcomeOK, Duration: 5 * time.Second, Bytes: 1 << 30,
	}})

	emit(obs.Event{
		Time: at(6), Cycle: testCycle, Step: "ENSURE_IMAGE", Kind: obs.KindActionEnded,
		Action: &obs.ActionEvent{
			Name: obs.ActionWaitForPull, Outcome: obs.OutcomeOK,
			Attrs: []obs.Attr{{Key: obs.AttrPullID, Value: pull.ID}},
		},
	})

	var root, waitForPull tracetest.SpanStub
	var children []tracetest.SpanStub
	for _, s := range exp.GetSpans() {
		switch s.Name {
		case "runny.pull":
			root = s
		case "http registry.token", "http registry.blob":
			children = append(children, s)
		case "cycle.step.action wait-for-pull":
			waitForPull = s
		}
	}
	if root.Name == "" {
		t.Fatal("no runny.pull root span found")
	}
	if len(children) != 2 {
		t.Fatalf("got %d client children under the pull, want 2", len(children))
	}
	for _, c := range children {
		if c.Parent.SpanID() != root.SpanContext.SpanID() {
			t.Errorf("%s parent = %x, want the pull root %x", c.Name, c.Parent.SpanID(), root.SpanContext.SpanID())
		}
	}

	if got := attrString(root.Attributes, "runny.outcome"); got != string(obs.OutcomeOK) {
		t.Errorf("pull root outcome = %q, want ok", got)
	}
	if got := attrInt64(root.Attributes, "runny.pull.bytes"); got != 1<<30 {
		t.Errorf("pull root bytes = %d, want %d", got, int64(1<<30))
	}
	if got := attrString(root.Attributes, "runny.progress.last"); got != "pulled 1 GiB at 500 MiB/s" {
		t.Errorf("pull root progress.last = %q", got)
	}
	if root.Status.Code == codes.Error {
		t.Error("a successful pull's root must not carry error status")
	}

	if waitForPull.Name == "" {
		t.Fatal("no cycle.step.action wait-for-pull span found")
	}
	rootID := attrString(root.Attributes, "runny.pull.id")
	waitID := attrString(waitForPull.Attributes, obs.AttrPullID)
	if rootID == "" || rootID != waitID {
		t.Errorf("pull ids: root=%q wait-for-pull=%q, want equal and non-empty", rootID, waitID)
	}
}

// A failed pull's root carries error status and the failure text — the same
// rule a failed step or action span follows.
func TestTraceConsumerPullFailureStatus(t *testing.T) {
	emit, exp := newTestAssembler(t)

	pull := &obs.PullRef{ID: "pull-fail", Ref: "ghcr.io/x", Digest: "sha256:x", Started: at(0)}
	emit(obs.Event{Time: at(0), Pull: pull, Kind: obs.KindPullStarted})
	emit(obs.Event{Time: at(1), Pull: pull, Kind: obs.KindPullFinished, PullInfo: &obs.PullEvent{
		Outcome: obs.OutcomeError, Error: "registry 503", Duration: time.Second,
	}})

	spans := exp.GetSpans()
	if len(spans) != 1 || spans[0].Name != "runny.pull" {
		t.Fatalf("got %d spans, want exactly 1 runny.pull root", len(spans))
	}
	root := spans[0]
	if root.Status.Code != codes.Error || root.Status.Description != "registry 503" {
		t.Errorf("pull root status = %+v, want Error \"registry 503\"", root.Status)
	}
}

// A pull abandoned before a terminal outcome (KindPullAbandoned, not
// KindPullFinished) still closes its root span — error status, no
// outcome/bytes attributes since there was never a result — and evicts the
// map entry, so a stray later event for the same pull id is a no-op instead
// of reopening or double-processing it.
func TestTraceConsumerPullAbandonedClosesRootAndEvicts(t *testing.T) {
	emit, exp := newTestAssembler(t)

	pull := &obs.PullRef{ID: "pull-abandoned", Ref: "ghcr.io/x", Digest: "sha256:x", Started: at(0)}
	emit(obs.Event{Time: at(0), Pull: pull, Kind: obs.KindPullStarted})
	emit(obs.Event{Time: at(1), Pull: pull, Kind: obs.KindDetail, Detail: &obs.DetailEvent{Text: "pulled 500 MiB"}})
	emit(obs.Event{Time: at(2), Pull: pull, Kind: obs.KindPullAbandoned})
	// A stray event after abandonment must not reopen or re-end the span.
	emit(obs.Event{Time: at(3), Pull: pull, Kind: obs.KindPullFinished, PullInfo: &obs.PullEvent{Outcome: obs.OutcomeOK}})

	spans := exp.GetSpans()
	if len(spans) != 1 || spans[0].Name != "runny.pull" {
		t.Fatalf("got %d spans, want exactly 1 runny.pull root", len(spans))
	}
	root := spans[0]
	if root.Status.Code != codes.Error {
		t.Errorf("abandoned pull root status = %v, want Error", root.Status.Code)
	}
	if got := attrString(root.Attributes, "runny.outcome"); got != "" {
		t.Errorf("abandoned pull root carries runny.outcome = %q, want none (no result to report)", got)
	}
	if got := attrString(root.Attributes, "runny.progress.last"); got != "pulled 500 MiB" {
		t.Errorf("abandoned pull root progress.last = %q, want the last detail before abandonment", got)
	}
	if root.EndTime.UTC() != at(2).UTC() {
		t.Errorf("abandoned pull root end time = %v, want %v (the abandonment time, not the stray KindPullFinished)", root.EndTime, at(2))
	}
}

// Two puller instances for the same image directory share the exact same
// deterministic PullRef.ID (a successor can start the instant the last
// subscriber leaves, before the predecessor's own goroutine notices
// cancellation and emits its terminal event). pullStarted closes the
// predecessor's span out as "superseded" the moment the successor starts;
// the predecessor's own stale terminal event, arriving later, must not
// touch or re-end the successor's live span — only the successor's own
// events (carrying its own *PullRef) can.
func TestTraceConsumerPullSuccessorSurvivesStalePredecessorEvent(t *testing.T) {
	emit, exp := newTestAssembler(t)

	predecessor := &obs.PullRef{ID: "pull-shared", Ref: "ghcr.io/x", Digest: "sha256:x", Started: at(0)}
	successor := &obs.PullRef{ID: "pull-shared", Ref: "ghcr.io/x", Digest: "sha256:x", Started: at(1)}

	emit(obs.Event{Time: at(0), Pull: predecessor, Kind: obs.KindPullStarted})
	emit(obs.Event{Time: at(1), Pull: successor, Kind: obs.KindPullStarted}) // supersedes predecessor's entry

	// The predecessor's own stale terminal event must not touch the
	// successor's now-installed entry.
	emit(obs.Event{Time: at(2), Pull: predecessor, Kind: obs.KindPullAbandoned})

	// The successor's own traffic and finish must land normally.
	emit(obs.Event{Time: at(3), Pull: successor, Kind: obs.KindHTTP, HTTP: &obs.HTTPEvent{
		Class: obs.HTTPRegistryBlob, Method: "GET", Host: "registry.example", Status: 200, Duration: time.Second,
	}})
	emit(obs.Event{Time: at(5), Pull: successor, Kind: obs.KindPullFinished, PullInfo: &obs.PullEvent{
		Outcome: obs.OutcomeOK, Duration: 4 * time.Second, Bytes: 100,
	}})

	var roots []tracetest.SpanStub
	var children int
	for _, s := range exp.GetSpans() {
		switch s.Name {
		case "runny.pull":
			roots = append(roots, s)
		case "http registry.blob":
			children++
		}
	}
	if len(roots) != 2 {
		t.Fatalf("got %d runny.pull spans, want exactly 2 (predecessor + successor, both closed)", len(roots))
	}
	if children != 1 {
		t.Fatalf("got %d client children, want 1 (the successor's HTTP traffic, not dropped as stray)", children)
	}

	var predecessorSpan, successorSpan tracetest.SpanStub
	for _, s := range roots {
		if s.EndTime.UTC() == at(1).UTC() {
			predecessorSpan = s
		} else {
			successorSpan = s
		}
	}
	if predecessorSpan.Status.Code != codes.Error || predecessorSpan.Status.Description != "superseded by a new pull for the same image" {
		t.Errorf("predecessor span status = %+v, want Error \"superseded...\"", predecessorSpan.Status)
	}
	if got := attrString(successorSpan.Attributes, "runny.outcome"); got != string(obs.OutcomeOK) {
		t.Errorf("successor span outcome = %q, want ok (its own real finish, not the predecessor's stale abandon)", got)
	}
	if successorSpan.EndTime.UTC() != at(5).UTC() {
		t.Errorf("successor span end time = %v, want %v (its own finish, not the predecessor's stale abandon at t=2)", successorSpan.EndTime, at(5))
	}
}

// KindHTTP/KindDetail for a pull whose KindPullStarted was never seen (a
// stray event, or a pull that already finished) is a no-op — the same
// stray-event tolerance withCycle/withStep give cycle-scoped events.
func TestTraceConsumerStrayPullEventsAreNoop(t *testing.T) {
	emit, exp := newTestAssembler(t)

	pull := &obs.PullRef{ID: "pull-stray"}
	emit(obs.Event{Time: at(0), Pull: pull, Kind: obs.KindHTTP, HTTP: &obs.HTTPEvent{Class: obs.HTTPRegistryBlob}})
	emit(obs.Event{Time: at(0), Pull: pull, Kind: obs.KindDetail, Detail: &obs.DetailEvent{Text: "x"}})
	emit(obs.Event{Time: at(0), Pull: pull, Kind: obs.KindPullFinished, PullInfo: &obs.PullEvent{Outcome: obs.OutcomeOK}})

	if spans := exp.GetSpans(); len(spans) != 0 {
		t.Fatalf("stray pull events (no KindPullStarted seen) produced %d spans, want 0", len(spans))
	}
}

// KindTarballDone is cycle-scoped and carries no trace rendering of its own
// (metrics-only, per internal/telemetry/metrics.go) — feeding one through
// must not produce a span.
func TestTraceConsumerIgnoresTarballDone(t *testing.T) {
	emit, exp := newTestAssembler(t)
	emit(obs.Event{Time: at(0), Cycle: testCycle, Kind: obs.KindTarballDone, Tarball: &obs.TarballEvent{
		Outcome: obs.OutcomeOK, Duration: time.Second,
	}})
	if spans := exp.GetSpans(); len(spans) != 0 {
		t.Fatalf("KindTarballDone produced %d spans, want 0", len(spans))
	}
}
