package statemachine

import (
	"slices"
	"sync"

	"github.com/bojanrajkovic/runny/internal/home"
)

// statusCell is the slot's mutex-guarded live-status core: the Status
// snapshot, the watcher list, the pause flag, and the failure streak. It is
// the one home for everything the slot's status lock guards, for the slot's
// entire lifetime, kept separate so fsm.go stays pure control flow (mirrors
// io.go's split for the same reason). As a rule Slot reaches c.mu only
// through a cell method; finishCycle's joint failures+Status write is the one
// carried-over exception (a raw lock, documented at its call site) — a later
// refactor (the publish seam) is the planned home for it.
type statusCell struct {
	mu       sync.Mutex
	status   Status
	onChange []func(Status)
	failures uint32
	paused   bool
}

// newStatusCell seeds the slot-constant identity fields (Slot, Pool, Image)
// so a slot that hasn't transitioned yet still renders a complete row.
func newStatusCell(name string, pool home.PoolConfig) *statusCell {
	return &statusCell{status: Status{Slot: name, Pool: pool.Name, Image: pool.Image}}
}

// snapshot returns the current status under the lock.
func (c *statusCell) snapshot() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

// onChangeAppend registers a status listener.
func (c *statusCell) onChangeAppend(fn func(Status)) {
	c.mu.Lock()
	c.onChange = append(c.onChange, fn)
	c.mu.Unlock()
}

// notify calls every listener with snap (callers must not hold the lock).
func (c *statusCell) notify(fns []func(Status), snap Status) {
	for _, fn := range fns {
		fn(snap)
	}
}

// update runs mut under the lock and returns the resulting snapshot and
// listener list. It does not itself notify: some callers broadcast the
// result immediately, others deliberately don't (a value published moments
// before the next setState's broadcast would otherwise double-fire).
func (c *statusCell) update(mut func(*Status)) (Status, []func(Status)) {
	c.mu.Lock()
	if mut != nil {
		mut(&c.status)
	}
	snap := c.status
	fns := slices.Clone(c.onChange)
	c.mu.Unlock()
	return snap, fns
}

// setDetailIfChanged sets Detail and returns (snapshot, listeners, true) iff
// detail differs from the current value, or (Status{}, nil, false) when it's
// unchanged — a no-op detail update must not fire a spurious watch notify.
func (c *statusCell) setDetailIfChanged(detail string) (Status, []func(Status), bool) {
	c.mu.Lock()
	if c.status.Detail == detail {
		c.mu.Unlock()
		return Status{}, nil, false
	}
	c.status.Detail = detail
	snap := c.status
	fns := slices.Clone(c.onChange)
	c.mu.Unlock()
	return snap, fns, true
}

func (c *statusCell) isPaused() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.paused
}

// recentCommandIDCap bounds the per-slot applied-command history. Generous
// relative to the client's confirm→catch window: an id only needs to survive
// from the snapshot that carries it until the client polls, so a handful is
// plenty, and the cap exists only to keep an always-paused slot's history
// from growing without bound.
const recentCommandIDCap = 16

// setPaused applies a pause/resume and republishes the slot status. cmdID is
// the applying command's correlation id (empty for daemon-internal
// re-issues); a non-empty id is appended to Status.RecentAppliedCommandIDs so
// the control surface can confirm that specific command by membership. An
// empty cmdID appends nothing, so a coalesced status stream never drops a
// client's acknowledgement out of the history.
func (c *statusCell) setPaused(p bool, cmdID string) {
	c.mu.Lock()
	c.paused = p
	c.status.Paused = p
	if cmdID != "" {
		c.status.RecentAppliedCommandIDs = appendBounded(c.status.RecentAppliedCommandIDs, cmdID, recentCommandIDCap)
	}
	snap := c.status
	fns := slices.Clone(c.onChange)
	c.mu.Unlock()
	c.notify(fns, snap)
}

// appendBounded returns ids with id appended, retaining at most limit entries
// (oldest evicted). slices.Clone always allocates a fresh backing array, even
// when the sliced region is the full input, so a status value already
// snapshotted under the lock keeps its own stable slice — a later append must
// not mutate an array a prior snapshot still references.
func appendBounded(ids []string, id string, limit int) []string {
	return append(slices.Clone(ids[max(0, len(ids)-limit+1):]), id)
}

// failureCount returns the current failure streak.
func (c *statusCell) failureCount() uint32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.failures
}
