package obs

import (
	"context"
	"testing"
	"time"
)

func testPull() PullRef {
	return PullRef{ID: "abc123", Ref: "ghcr.io/x@sha256:d34d", Digest: "sha256:d34d", Started: time.Now()}
}

// A pull-scoped event carries Event.Pull, not Event.Cycle — the two scope
// kinds are mutually exclusive by construction.
func TestPullScopeStampsPullNotCycle(t *testing.T) {
	var got Event
	pull := testPull()
	ctx := WithPull(context.Background(), func(e Event) { got = e }, pull)

	Emit(ctx, Event{Kind: KindPullStarted})

	if got.Pull == nil || *got.Pull != pull {
		t.Fatalf("event Pull = %+v, want %+v", got.Pull, pull)
	}
	if got.Cycle != (CycleRef{}) {
		t.Fatalf("pull-scoped event should have a zero Cycle, got %+v", got.Cycle)
	}
}

// A pull scope's Seq counter is its own — independent of any cycle scope's,
// the same isolation TestSeqIsPerCycleNotGlobal checks between two cycles.
func TestPullSeqIndependentOfCycleScope(t *testing.T) {
	var pullSeqs, cycleSeqs []uint64
	pctx := WithPull(context.Background(), func(e Event) { pullSeqs = append(pullSeqs, e.Seq) }, testPull())
	cctx := WithCycle(context.Background(), func(e Event) { cycleSeqs = append(cycleSeqs, e.Seq) }, testCycle())

	Emit(pctx, Event{Kind: KindPullStarted})
	Emit(cctx, Event{Kind: KindCycleStarted})
	Emit(pctx, Event{Kind: KindPullFinished, PullInfo: &PullEvent{Outcome: OutcomeOK}})

	if len(pullSeqs) != 2 || pullSeqs[0] != 1 || pullSeqs[1] != 2 {
		t.Fatalf("pull seqs = %v, want [1 2]", pullSeqs)
	}
	if len(cycleSeqs) != 1 || cycleSeqs[0] != 1 {
		t.Fatalf("cycle seqs = %v, want [1]", cycleSeqs)
	}
}

// A pull scope with a nil emitter must degrade to a safe no-op, the same
// contract WithCycle gives.
func TestWithPullNilEmitterIsNoop(t *testing.T) {
	ctx := WithPull(context.Background(), nil, testPull())
	Emit(ctx, Event{Kind: KindPullStarted}) // must not panic
}
