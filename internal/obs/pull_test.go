package obs

import (
	"context"
	"testing"
)

func testPull() PullRef {
	return PullRef{ID: "abc123", Ref: "ghcr.io/x@sha256:d34d", Digest: "sha256:d34d"}
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

// A pull scope with a nil emitter must degrade to a safe no-op, the same
// contract WithCycle gives.
func TestWithPullNilEmitterIsNoop(t *testing.T) {
	ctx := WithPull(context.Background(), nil, testPull())
	Emit(ctx, Event{Kind: KindPullStarted}) // must not panic
}
