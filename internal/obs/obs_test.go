package obs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bojanrajkovic/runny/internal/bounded"
)

func testCycle() CycleRef {
	return CycleRef{InstancePrefix: "host-ab12cd34", Slot: "slot-0", CycleID: "deadbeef", Started: time.Now()}
}

// Seq is per-cycle monotonic: every event emitted through one cycle scope
// hands out a strictly increasing sequence.
func TestSeqMonotonicPerCycle(t *testing.T) {
	var got []uint64
	emit := func(e Event) { got = append(got, e.Seq) }

	ctx := WithStep(WithCycle(context.Background(), emit, testCycle()), "BOOT")
	for range 3 {
		_ = Action(ctx, "step", func(context.Context) error { return nil })
	}

	if len(got) != 6 { // ActionStarted + ActionEnded per call
		t.Fatalf("got %d events, want 6", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Fatalf("Seq not monotonic: %v", got)
		}
	}
}

// The counter belongs to the cycle scope: step scopes derived from it share
// it, so Seq stays strictly monotonic across the whole cycle no matter how
// many step scopes the FSM layers on, interleaving Emit and Action.
func TestSeqSharedAcrossStepScopes(t *testing.T) {
	var got []Event
	emit := func(e Event) { got = append(got, e) }

	cctx := WithCycle(context.Background(), emit, testCycle())
	boot := WithStep(cctx, "BOOT")
	prov := WithStep(cctx, "PROVISION")

	Emit(cctx, Event{Kind: KindCycleStarted})
	Emit(boot, Event{Kind: KindStepEntered, StepInfo: &StepEvent{State: "BOOT"}})
	_ = Action(boot, "clone", func(context.Context) error { return nil })
	Emit(boot, Event{Kind: KindStepLeft, StepInfo: &StepEvent{State: "BOOT", Outcome: OutcomeOK}})
	Emit(prov, Event{Kind: KindStepEntered, StepInfo: &StepEvent{State: "PROVISION"}})
	Emit(prov, Event{Kind: KindDetail, Detail: &DetailEvent{Text: "2.1 GiB at 41 MiB/s"}})
	_ = Action(prov, "install-runner", func(context.Context) error { return nil })
	Emit(cctx, Event{Kind: KindCycleFinished, Finish: &FinishEvent{Result: "success"}})

	if len(got) != 10 {
		t.Fatalf("got %d events, want 10", len(got))
	}
	for i, e := range got {
		if e.Seq != uint64(i+1) {
			t.Fatalf("event %d has Seq %d, want %d (per-cycle order broken): %+v", i, e.Seq, i+1, got)
		}
		if e.Cycle.CycleID != "deadbeef" {
			t.Fatalf("event %d lost cycle identity: %+v", i, e)
		}
		if e.Time.IsZero() {
			t.Fatalf("event %d has zero Time", i)
		}
	}
	// Cycle-scope events are stepless; step-scope events — Emit'd (the
	// Detail at index 6) and Action-emitted alike — carry their step.
	if got[0].Step != "" || got[9].Step != "" {
		t.Fatalf("cycle-scope events should have empty Step: %q / %q", got[0].Step, got[9].Step)
	}
	if got[2].Step != "BOOT" || got[6].Step != "PROVISION" || got[7].Step != "PROVISION" {
		t.Fatalf("step stamping wrong: action=%q detail=%q action=%q", got[2].Step, got[6].Step, got[7].Step)
	}
}

// Emit on a context that never saw WithCycle, and on a scope with a nil
// emitter, must be a safe no-op.
func TestEmitWithoutScopeOrEmitterIsNoop(t *testing.T) {
	Emit(context.Background(), Event{Kind: KindDetail, Detail: &DetailEvent{Text: "x"}}) // must not panic

	nilCtx := WithCycle(context.Background(), nil, testCycle())
	Emit(nilCtx, Event{Kind: KindDetail, Detail: &DetailEvent{Text: "x"}}) // must not panic

	// WithStep without a scope leaves the context untouched.
	base := context.Background()
	if got := WithStep(base, "BOOT"); got != base {
		t.Fatal("WithStep on a scope-less context should return it unchanged")
	}
}

// A nil emitter must never be called and Action must still run fn and
// return its result — the no-op path used when telemetry is unconfigured.
func TestNilEmitterIsNoop(t *testing.T) {
	ctx := WithCycle(context.Background(), nil, testCycle())

	called := false
	err := Action(ctx, "step", func(context.Context) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Fatal("fn was not called")
	}
}

// A context that never went through WithCycle must degrade to a plain fn()
// call: zero events, no panic. Domain packages that don't know telemetry
// exists must be able to call Action safely on any context.
func TestScopelessContextIsPlainCall(t *testing.T) {
	called := false
	err := Action(context.Background(), "step", func(context.Context) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Fatal("fn was not called")
	}
}

// Action must emit ActionStarted then ActionEnded, and ActionEnded must
// carry outcome, error, and a non-negative duration.
func TestActionCapturesOutcomeErrorDuration(t *testing.T) {
	var events []Event
	emit := func(e Event) { events = append(events, e) }
	ctx := WithStep(WithCycle(context.Background(), emit, testCycle()), "PROVISION")

	sentinel := errors.New("boom")
	err := Action(ctx, "install-runner", func(context.Context) error {
		time.Sleep(time.Millisecond)
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Action did not return fn's error: %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}

	started, ended := events[0], events[1]
	if started.Kind != KindActionStarted {
		t.Fatalf("first event kind = %v, want ActionStarted", started.Kind)
	}
	if started.Action == nil || started.Action.Name != "install-runner" {
		t.Fatalf("ActionStarted payload = %+v", started.Action)
	}
	if started.Step != "PROVISION" {
		t.Fatalf("ActionStarted step = %q, want PROVISION", started.Step)
	}

	if ended.Kind != KindActionEnded {
		t.Fatalf("second event kind = %v, want ActionEnded", ended.Kind)
	}
	if ended.Action == nil {
		t.Fatal("ActionEnded payload is nil")
	}
	if ended.Action.Outcome != OutcomeError {
		t.Fatalf("ActionEnded outcome = %q, want error", ended.Action.Outcome)
	}
	if ended.Action.Error != sentinel.Error() {
		t.Fatalf("ActionEnded error = %q, want %q", ended.Action.Error, sentinel.Error())
	}
	if ended.Action.Duration < time.Millisecond {
		t.Fatalf("ActionEnded duration = %v, want >= 1ms", ended.Action.Duration)
	}
}

// A successful fn must produce OutcomeOK and an empty error string.
func TestActionSuccessOutcomeOK(t *testing.T) {
	var events []Event
	emit := func(e Event) { events = append(events, e) }
	ctx := WithCycle(context.Background(), emit, testCycle())

	if err := Action(ctx, "clone", func(context.Context) error { return nil }); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ended := events[1]
	if ended.Action.Outcome != OutcomeOK {
		t.Fatalf("outcome = %q, want ok", ended.Action.Outcome)
	}
	if ended.Action.Error != "" {
		t.Fatalf("error = %q, want empty", ended.Action.Error)
	}
}

// Scope must survive a bounded.Context wrap (WithTimeout), since every
// guest/network seam takes bounded.Context, not context.Context. This is
// the load-bearing property: bounded.Context.Value delegates to the parent,
// so a scope set before bounding still reaches Action after.
func TestScopePropagatesThroughBoundedContext(t *testing.T) {
	var events []Event
	emit := func(e Event) { events = append(events, e) }

	scoped := WithStep(WithCycle(context.Background(), emit, testCycle()), "AWAIT_SSH")
	bctx, cancel := bounded.WithTimeout(scoped, time.Second)
	defer cancel()

	if err := Action(bctx, "dial", func(context.Context) error { return nil }); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("got %d events through bounded.Context, want 2", len(events))
	}
	if events[0].Cycle.CycleID != "deadbeef" {
		t.Fatalf("cycle identity lost across bounded.Context: %+v", events[0].Cycle)
	}
}

// Every emitted event must carry the cycle identity set at WithCycle time.
func TestEventCarriesCycleIdentity(t *testing.T) {
	var got Event
	emit := func(e Event) { got = e }
	cycle := testCycle()
	ctx := WithCycle(context.Background(), emit, cycle)

	_ = Action(ctx, "run", func(context.Context) error { return nil })

	if got.Cycle != cycle {
		t.Fatalf("event cycle = %+v, want %+v", got.Cycle, cycle)
	}
}

// Two independent cycle scopes (two cycles/slots) must not share a Seq
// counter.
func TestSeqIsPerCycleNotGlobal(t *testing.T) {
	var seqsA, seqsB []uint64
	emitA := func(e Event) { seqsA = append(seqsA, e.Seq) }
	emitB := func(e Event) { seqsB = append(seqsB, e.Seq) }

	ctxA := WithCycle(context.Background(), emitA, testCycle())
	ctxB := WithCycle(context.Background(), emitB, testCycle())

	_ = Action(ctxA, "a1", func(context.Context) error { return nil })
	_ = Action(ctxB, "b1", func(context.Context) error { return nil })
	_ = Action(ctxA, "a2", func(context.Context) error { return nil })

	if len(seqsA) != 4 || len(seqsB) != 2 {
		t.Fatalf("seqsA=%v seqsB=%v", seqsA, seqsB)
	}
	// A's second action pair must still be monotonic within A, unaffected by B.
	if seqsA[2] <= seqsA[1] || seqsA[3] <= seqsA[2] {
		t.Fatalf("seqsA not monotonic: %v", seqsA)
	}
	if seqsB[0] != 1 || seqsB[1] != 2 {
		t.Fatalf("seqsB should start at 1 independently: %v", seqsB)
	}
}
