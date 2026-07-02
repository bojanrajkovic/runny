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

// Seq is per-cycle monotonic: two scopes built from the same cycle start
// point must hand out a strictly increasing sequence across every event
// emitted through either of them.
func TestSeqMonotonicPerCycle(t *testing.T) {
	var got []uint64
	emit := func(e Event) { got = append(got, e.Seq) }

	ctx := WithScope(context.Background(), emit, testCycle(), "BOOT")
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

// A nil emitter must never be called and Action must still run fn and
// return its result — the no-op path used when telemetry is unconfigured.
func TestNilEmitterIsNoop(t *testing.T) {
	ctx := WithScope(context.Background(), nil, testCycle(), "BOOT")

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

// A context that never went through WithScope must degrade to a plain fn()
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
	ctx := WithScope(context.Background(), emit, testCycle(), "PROVISION")

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
	if started.Action.Step != "PROVISION" {
		t.Fatalf("ActionStarted step = %q, want PROVISION", started.Action.Step)
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
	ctx := WithScope(context.Background(), emit, testCycle(), "BOOT")

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

	scoped := WithScope(context.Background(), emit, testCycle(), "AWAIT_SSH")
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

// Every emitted event must carry the cycle identity set at WithScope time.
func TestEventCarriesCycleIdentity(t *testing.T) {
	var got Event
	emit := func(e Event) { got = e }
	cycle := testCycle()
	ctx := WithScope(context.Background(), emit, cycle, "JOB")

	_ = Action(ctx, "run", func(context.Context) error { return nil })

	if got.Cycle != cycle {
		t.Fatalf("event cycle = %+v, want %+v", got.Cycle, cycle)
	}
}

// Two independent scopes (two cycles/slots) must not share a Seq counter.
func TestSeqIsPerScopeNotGlobal(t *testing.T) {
	var seqsA, seqsB []uint64
	emitA := func(e Event) { seqsA = append(seqsA, e.Seq) }
	emitB := func(e Event) { seqsB = append(seqsB, e.Seq) }

	ctxA := WithScope(context.Background(), emitA, testCycle(), "BOOT")
	ctxB := WithScope(context.Background(), emitB, testCycle(), "BOOT")

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
