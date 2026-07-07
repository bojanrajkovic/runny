package obs

import (
	"context"
	"time"
)

// PullRef identifies one shared image pull — the subject of a pull scope. A
// pull belongs to no single cycle (many slots can subscribe to the same
// underlying pull), so it gets its own scope kind alongside CycleRef rather
// than borrowing cycle identity.
type PullRef struct {
	// ID is pullID(dir): the correlation handle cycles already carry via
	// AttrPullID on their wait-for-pull action.
	ID     string
	Ref    string
	Digest string
}

const (
	// KindPullStarted/KindPullFinished bracket one shared image pull's
	// lifetime. Identity travels on Event.Pull, not a payload — everything
	// worth knowing at start is already on PullRef.
	KindPullStarted  Kind = "pull_started"
	KindPullFinished Kind = "pull_finished"
	// KindPullAbandoned closes a pull that never reached a terminal
	// outcome (its last subscriber left before an attempt resolved) —
	// distinct from KindPullFinished so a metrics consumer that only
	// switches on the latter still records nothing for it, matching the
	// no-fabricated-outcome rule. It exists purely so a consumer that
	// opened something at KindPullStarted has a signal to close it with;
	// carries no payload.
	KindPullAbandoned Kind = "pull_abandoned"
)

// PullEvent is the payload for KindPullFinished.
type PullEvent struct {
	Outcome  Outcome
	Error    string
	Duration time.Duration
	Bytes    int64
}

// WithPull establishes the observability scope for one shared image pull:
// every Emit on the returned context (or a derived context) emits through
// emit with Event.Pull set to pull instead of Event.Cycle. emit may be nil,
// degrading every Emit on this scope to a no-op — the same contract
// WithCycle gives cycle scopes.
func WithPull(ctx context.Context, emit Emitter, pull PullRef) context.Context {
	return context.WithValue(ctx, scopeKey{}, newScope(emit, CycleRef{}, &pull))
}
