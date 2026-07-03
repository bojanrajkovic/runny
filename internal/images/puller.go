package images

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bojanrajkovic/runny/internal/bounded"
	"github.com/bojanrajkovic/runny/internal/diskfree"
	"github.com/bojanrajkovic/runny/internal/oci"
	"github.com/bojanrajkovic/runny/internal/tart"
)

// defaultDiskHoldBudget bounds how long the shared puller waits for an operator
// to free disk space before handing the deterministic failure back to each
// subscriber's FSM (which applies its own backoff and re-enters ENSURE_IMAGE,
// spawning a fresh puller that resumes waiting). It is CUMULATIVE across the
// puller's lifetime — re-attempts after transient headroom do not reset it — so
// a disk oscillating at the threshold can't keep the puller alive forever.
// Sized as a standalone healthy magnitude — long enough for a human to clear
// space — NOT derived from any other budget. ponytail: 5m, tune if real
// operators need longer. Copied into each puller (holdBudget/pollInterval) so
// there is no shared mutable global for tests to race.
const (
	defaultDiskHoldBudget   = 5 * time.Minute
	defaultDiskPollInterval = 15 * time.Second
)

// ensureResult is the terminal outcome of an image pull, delivered to every
// subscriber that shared the puller.
type ensureResult struct {
	digest string
	bundle tart.Bundle
	err    error
}

// subscription is one slot's handle on a shared pull. done is buffered (size 1)
// and receives exactly one result; Ensure selects on it against the slot ctx.
type subscription struct {
	done chan ensureResult
}

// imagePuller owns the single in-flight pull of one image bundle directory plus
// the bounded, paced retry of its one deterministic failure (disk headroom),
// and broadcasts the terminal outcome to every current subscriber. One per
// destDir, tracked in pullerRegistry. The byte-pull is still serialized inside
// oci.PullTo (pullLocks) as defense in depth; the actor is what shares the
// OUTCOME — concurrent slots no longer each re-run a doomed pull (issue #125).
type imagePuller struct {
	destDir string
	ref     oci.Ref // pinned to the resolved digest
	stall   time.Duration
	log     *slog.Logger
	metrics *Metrics // nil-safe; records the terminal pull outcome

	// startedAt anchors the pull-duration metric; pullBytes accumulates
	// transferred bytes across attempts (fed by realAttempt's progress
	// callback, which runs on this goroutine — atomic because tests and
	// future callers may feed from elsewhere).
	startedAt time.Time
	pullBytes atomic.Int64

	// ctx is the puller's lifetime context, cancelled when the last subscriber
	// leaves (or on terminal). Not a bounded.Context: the actual network ops get
	// their bounds from a per-attempt stall watch and the disk hold from
	// holdBudget; this is a deliberate lifetime context, not a bounded one.
	ctx    context.Context
	cancel context.CancelFunc

	// Seams, set by acquireImagePull to the real implementations and stubbed in
	// tests. attempt runs one bounded pull; diskFree probes the destination
	// filesystem's free space.
	attempt  func(ctx context.Context) (string, error)
	diskFree func(dir string) (uint64, error)

	// holdBudget/pollInterval are the disk-hold tunables, copied from the
	// package defaults at construction (tests set smaller ones on the proto).
	holdBudget   time.Duration
	pollInterval time.Duration

	mu         sync.Mutex
	subs       map[*subscription]func(string) // sub -> its live-status reporter
	terminal   *ensureResult                  // set once; nil while pulling
	stopped    bool                           // terminal reached or last-out; never reused
	lastDetail string                         // replayed to a late joiner
}

var (
	pullerRegistryMu sync.Mutex
	pullerRegistry   = map[string]*imagePuller{}
)

// acquireImagePull subscribes report to the shared pull of dir, starting the
// puller if this is the first subscriber. The returned release MUST be called
// (defer it): it drops the subscription and, when it was the last, stops the
// puller. ref must be digest-pinned; all subscribers of one dir necessarily
// resolved the same digest because dir is content-addressed
// (home.ImageBundleDir embeds the digest).
func (e *Ensurer) acquireImagePull(dir string, ref oci.Ref, report func(string)) (*subscription, func()) {
	proto := &imagePuller{
		destDir: dir, ref: ref, stall: e.StallBudget, log: e.log(), metrics: e.Metrics,
		holdBudget: defaultDiskHoldBudget, pollInterval: defaultDiskPollInterval,
	}
	proto.attempt = proto.realAttempt
	proto.diskFree = diskfree.AvailableBytes
	return acquirePuller(dir, report, proto)
}

// acquirePuller registers proto and starts it if dir has no live puller, then
// subscribes report. proto is consumed only when this is the first subscriber;
// otherwise the existing puller is reused. Split out so tests can supply a proto
// with stubbed seams. Lock order pullerRegistryMu -> p.mu.
func acquirePuller(dir string, report func(string), proto *imagePuller) (*subscription, func()) {
	pullerRegistryMu.Lock()
	p := pullerRegistry[dir]
	if p == nil {
		pctx, cancel := context.WithCancel(context.Background())
		proto.ctx = pctx
		proto.cancel = cancel
		proto.subs = map[*subscription]func(string){}
		proto.startedAt = time.Now()
		p = proto
		pullerRegistry[dir] = p
		go p.run()
	}
	sub := &subscription{done: make(chan ensureResult, 1)}
	p.mu.Lock()
	// Registry invariant: a puller present in the registry is live — finish and
	// unsubscribe both delete it from the registry under pullerRegistryMu in the
	// same critical section that marks it stopped — so here terminal==nil and
	// stopped==false. Adding the subscriber before releasing pullerRegistryMu
	// means a finish that runs the instant we unlock still delivers to it.
	p.subs[sub] = report
	last := p.lastDetail
	p.mu.Unlock()
	pullerRegistryMu.Unlock()
	if last != "" && report != nil {
		report(last) // a late joiner sees the current wait immediately
	}
	return sub, func() { p.unsubscribe(sub) }
}

// unsubscribe drops sub and, when it was the last subscriber, stops the puller:
// cancel the lifetime ctx (interrupting any in-flight pull or hold) and delete
// the registry entry. Lock order is always pullerRegistryMu -> p.mu, matching
// acquireImagePull and finish, so the three never deadlock.
func (p *imagePuller) unsubscribe(sub *subscription) {
	pullerRegistryMu.Lock()
	p.mu.Lock()
	delete(p.subs, sub)
	if len(p.subs) == 0 && !p.stopped {
		p.stopped = true
		p.cancel()
		if pullerRegistry[p.destDir] == p {
			delete(pullerRegistry, p.destDir)
		}
	}
	p.mu.Unlock()
	pullerRegistryMu.Unlock()
}

// finish records the terminal outcome, removes the puller from the registry, and
// delivers the result to every current subscriber exactly once. Idempotent: a
// second call (e.g. the panic recover after a normal finish) is a no-op.
func (p *imagePuller) finish(res ensureResult) {
	pullerRegistryMu.Lock()
	p.mu.Lock()
	if p.terminal != nil {
		p.mu.Unlock()
		pullerRegistryMu.Unlock()
		return
	}
	p.terminal = &res
	p.stopped = true
	subs := p.subs
	p.subs = map[*subscription]func(string){}
	if pullerRegistry[p.destDir] == p {
		delete(pullerRegistry, p.destDir)
	}
	p.mu.Unlock()
	pullerRegistryMu.Unlock()
	p.cancel() // release the lifetime ctx; run() has returned by now
	// Only the winner of the terminal==nil guard reaches here, so the pull
	// metric records exactly once per underlying pull — subscriber count and
	// finish/panic double-calls can't inflate it.
	p.metrics.pullDone(outcomeOf(res.err), time.Since(p.startedAt), p.pullBytes.Load())
	for sub := range subs {
		sub.done <- res // buffered size 1, never blocks, delivered once
	}
}

// run is the puller goroutine: pull, and on the one deterministic failure (disk
// headroom) hold and retry within a bounded window, otherwise broadcast and
// stop. A panic is converted into a terminal error so subscribers can never
// hang waiting on a dead puller (the silent-failure shape this project kills).
func (p *imagePuller) run() {
	defer func() {
		if r := recover(); r != nil {
			p.finish(ensureResult{err: fmt.Errorf("image puller for %s panicked: %v", p.ref, r)})
		}
	}()
	// holdDeadline bounds the TOTAL time spent in disk holds across this puller's
	// life, set on the first deterministic failure and never reset — so a disk
	// that briefly clears (letting a re-attempt through) then re-fills can't keep
	// resetting a per-hold timer and hold the puller forever.
	var holdDeadline time.Time
	for {
		if p.ctx.Err() != nil {
			return // last subscriber left, or shutting down
		}
		dig, err := p.attempt(p.ctx)
		if err == nil {
			// Finish even if the last subscriber left mid-attempt: the bundle
			// really landed (the next cycle cache-hits it), so the terminal
			// outcome — and its pull metric — must exist. Nobody waits on the
			// broadcast (subs is empty), and finish's registry delete is
			// guarded, so a successor puller for the same dir is untouched.
			p.finish(ensureResult{digest: dig, bundle: tart.Bundle(p.destDir)})
			return
		}
		if p.ctx.Err() != nil {
			return // cancelled during the attempt; nobody is waiting
		}
		var dh *oci.DiskHeadroomError
		if errors.As(err, &dh) {
			if holdDeadline.IsZero() {
				holdDeadline = time.Now().Add(p.holdBudget)
			}
			if p.holdForDisk(dh, holdDeadline) {
				continue // headroom appeared within budget — re-attempt the pull
			}
			return // gave up (finish already called) or ctx cancelled
		}
		// Transient: re-attempting might succeed, so hand it back to each
		// subscriber's FSM, which owns transient retry via backoff.
		p.finish(ensureResult{err: err})
		return
	}
}

// realAttempt runs one stall-watched pull. The stall watch is built per attempt
// (a disk hold makes no progress, so it must not run under a stall watch that
// would trip on the legitimate silence). stallErr surfaces the stall cause on a
// killed transfer and preserves a wrapped *DiskHeadroomError via %w.
func (p *imagePuller) realAttempt(ctx context.Context) (string, error) {
	stall := bounded.NewStall()
	wctx, cancel := stall.Watch(ctx, p.stall)
	defer cancel()
	prog := newProgress(p.report, p.log, p.stall)
	defer prog.stop()
	client := oci.NewClient()
	client.Progress = func(n int64) {
		stall.Feed(n)
		prog.feed(n)
		p.pullBytes.Add(n)
	}
	dig, err := client.PullTo(wctx, p.ref, p.destDir)
	if err != nil {
		return "", stallErr(wctx, err, fmt.Sprintf("pulling %s", p.ref))
	}
	return dig, nil
}

// holdForDisk waits for disk headroom after a deterministic disk-guard refusal,
// up to the CUMULATIVE deadline (shared across re-attempts by the caller).
// Returns true once there is room (retry the pull); false if the deadline
// elapsed (finish already broadcast the give-up) or the ctx was cancelled.
//
// It waits one poll interval BEFORE re-probing — the attempt that sent us here
// already found the disk short, and gating the re-probe this way bounds the
// re-attempt rate to one per interval, so a disk oscillating at the threshold
// (free on a probe, short again by the time PullTo's guard re-checks) can't spin
// the pull into a tight registry-hammering loop. The deadline check is at the
// top, so even a disk that keeps clearing-then-refilling is bounded. The held
// status carries the remaining time so each tick is a distinct string that
// republishes past setDetail's identical-string dedup.
func (p *imagePuller) holdForDisk(dh *oci.DiskHeadroomError, deadline time.Time) bool {
	dir := filepath.Dir(p.destDir)
	t := time.NewTicker(p.pollInterval)
	defer t.Stop()
	for {
		if !time.Now().Before(deadline) {
			p.finish(ensureResult{err: fmt.Errorf(
				"pulling %s: %w (no headroom after %s)", p.ref, dh, p.holdBudget,
			)})
			return false
		}
		p.report(p.holdDetail(dir, dh, deadline))
		select {
		case <-p.ctx.Done():
			return false
		case <-t.C:
		}
		// Re-probe after the wait. A probe error is NOT a full disk — but it
		// still isn't headroom, so keep holding (the message names it); only a
		// positive reading clears the hold.
		if free, ferr := p.diskFree(dir); ferr == nil && free >= dh.NeedBytes() {
			return true
		}
	}
}

// holdDetail formats the live held-status line, surfacing a probe error
// distinctly from a genuine shortfall so a stat failure (e.g. the volume
// vanished mid-hold) isn't misattributed to headroom.
func (p *imagePuller) holdDetail(dir string, dh *oci.DiskHeadroomError, deadline time.Time) string {
	remaining := time.Until(deadline).Round(time.Second)
	free, ferr := p.diskFree(dir)
	if ferr != nil {
		return fmt.Sprintf(
			"image unavailable: cannot check disk free space (%v) — retrying, giving up in %s; shared by %d slot(s)",
			ferr, remaining, p.subscriberCount(),
		)
	}
	// "pull refused for disk headroom", not "insufficient": in the oscillation
	// case the probe can read free >= need while the pull's own guard still
	// refused (space went away between probe and pull), so don't assert free < need.
	return fmt.Sprintf(
		"image unavailable: pull refused for disk headroom (need %s, free %s) — retrying, giving up in %s; shared by %d slot(s)",
		oci.HumanBytes(int64(dh.NeedBytes())), oci.HumanBytes(int64(free)),
		remaining, p.subscriberCount(),
	)
}

// report fans a live status line out to every current subscriber. Subscribers
// are snapshotted under p.mu and the closures invoked OUTSIDE it: each reporter
// is a slot's setDetail, which takes the slot mutex, so calling it under p.mu
// would invert the lock order into slot.mu -> p.mu.
func (p *imagePuller) report(detail string) {
	p.mu.Lock()
	p.lastDetail = detail
	reporters := make([]func(string), 0, len(p.subs))
	for _, r := range p.subs {
		if r != nil {
			reporters = append(reporters, r)
		}
	}
	p.mu.Unlock()
	for _, r := range reporters {
		r(detail)
	}
}

func (p *imagePuller) subscriberCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.subs)
}
