package main

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bojanrajkovic/runny/internal/socket"
	"github.com/bojanrajkovic/runny/internal/statemachine"
)

// drainSlot is the slice of *statemachine.Slot the drainer needs, an
// interface so tests drive stubs on any OS.
type drainSlot interface {
	Command(statemachine.Command) bool
	Status() statemachine.Status
}

// drainer drives the fleet to stable idle and then hands the process to
// launchd for a cold start. Two causes share it (the wedge escalation and
// config reload): pause holds each slot in
// BACKOFF after its current cycle (a running job finishes first), recycle
// ends LISTENING without waiting out max-idle, and the daemon exits only
// once every slot is in a stable state — wedged, or paused in BACKOFF,
// which cannot start a job. Commands are RE-ISSUED on every status change
// until convergence: Slot.Command is non-blocking and drops on a full
// buffer, and backoffWait's timer-vs-pause select can consume a fired
// timer before a queued pause — an issue-once drain can stall on either.
// Re-issue makes convergence monotone (pause is idempotent; recycle while
// idle is a no-op).
type drainer struct {
	slots []drainSlot
	log   *slog.Logger
	stop  func() // the signal-context cancel
	// exitGate re-checks, locally, that the on-disk config the respawn will
	// load is one it would accept; nil = always ok. It receives the SHA-256
	// recorded at Start ("" for the wedge cause) so a mid-drain edit can be
	// WARNed about. Returning false HOLDS the exit: nothing is repaired —
	// the daemon declines to hand launchd a provably-bad config, stays up
	// drained, and a 30s ticker revalidates so fixing the file is
	// sufficient.
	exitGate func(acceptedSHA string) (ok bool, detail string)
	// retryEvery overrides the held-exit-gate revalidation interval (tests).
	retryEvery time.Duration
	// onProgress is called on each drain-progress event (a slot transition
	// while draining, or an exit-gate hold flip). Wired to the socket server's
	// watch fan-out so a following client's stall timer resets on real
	// progress without waiting out the 30s heartbeat. Nil = not wired (tests).
	onProgress func()

	// seq is the monotone drain-progress counter published as drain_seq. Bumped
	// only on real progress (never the heartbeat), so a frozen seq across
	// heartbeats is the signal of a stalled drain. Atomic: read off the RPC
	// path, written from FSM goroutines.
	seq atomic.Uint64
	// deferConfigParse is set by an UpgradeReload-caused drain; the exit gate
	// reads it to decide whether a config-parse failure may be deferred to the
	// respawn target's -test-config (a forward-only edit the old binary rejects).
	deferConfigParse atomic.Bool

	mu          sync.Mutex // control-plane frequency; a mutex beats CAS choreography
	reason      string     // empty = not draining; feeds GetStatus `draining`
	holdDetail  string     // non-empty while the exit gate is refusing
	acceptedSHA string     // config sha256 recorded at Start (reload only)
	exited      bool
	exitRunning bool   // one tryExit at a time
	retryStop   func() // stops the exit-gate retry ticker
}

// progress records one drain-progress event: it bumps the published counter
// and kicks the watch fan-out so followers see movement immediately. Safe from
// an FSM goroutine — the increment is atomic and onProgress is a non-blocking
// notify.
func (d *drainer) progress() {
	d.seq.Add(1)
	if d.onProgress != nil {
		d.onProgress()
	}
}

// setHold updates the exit-gate hold detail and records drain progress when the
// held state flips (empty<->non-empty). The flip is a real drain event that
// does NOT run through a slot's OnChange, so bumping here is what makes a hold
// set or release visible to followers before the next 30s heartbeat.
func (d *drainer) setHold(detail string) {
	d.mu.Lock()
	flipped := (d.holdDetail == "") != (detail == "")
	d.holdDetail = detail
	d.mu.Unlock()
	if flipped {
		d.progress()
	}
}

// stableStatus is the per-slot convergence predicate: the states that
// cannot start a job.
func stableStatus(st statemachine.Status) bool {
	return st.Wedged || (st.Paused && st.State == statemachine.StateBackoff)
}

// Start begins a drain. Returns false when one is already active: the
// first reason wins and keeps the status surface truthful about why slots
// are pausing.
func (d *drainer) Start(reason, configSHA string) bool {
	d.mu.Lock()
	if d.reason != "" {
		d.mu.Unlock()
		return false
	}
	d.reason = reason
	d.acceptedSHA = configSHA
	d.mu.Unlock()
	d.log.Error("draining fleet to idle, then restarting", "reason", reason)
	d.recheck()
	return true
}

// SetDeferConfigParse marks this drain as UpgradeReload-caused so the exit gate
// may defer a config-parse failure to the respawn target's -test-config. Call it
// BEFORE Start: Start rechecks convergence synchronously and can spawn tryExit
// (whose exit gate reads this flag) before Start returns, so setting it after
// would race an already-stable fleet. Setting it pre-Start is safe — the flag
// only gates the exit gate, which never runs until draining — and still covers
// the merge case (a wedge drain a later UpgradeReload joins mid-flight).
func (d *drainer) SetDeferConfigParse() {
	d.deferConfigParse.Store(true)
}

// UpdateAcceptedSHA records a newly-validated config hash onto an
// already-active drain (a second accepted reload, or a reload accepted during
// a wedge drain), so the exit gate compares the on-disk file against the most
// recently vetted version rather than the first cause's ("" for a wedge, or a
// now-superseded earlier reload). No-op when not draining.
func (d *drainer) UpdateAcceptedSHA(sha string) {
	d.mu.Lock()
	if d.reason != "" {
		d.acceptedSHA = sha
	}
	d.mu.Unlock()
}

// reasonLocked builds the drain reason with the exit-gate hold annotation
// appended while the gate is refusing. Caller holds d.mu.
func (d *drainer) reasonLocked() string {
	if d.reason == "" {
		return ""
	}
	if d.holdDetail != "" {
		return d.reason + " — exit held: " + d.holdDetail
	}
	return d.reason
}

// Reason reports the active drain reason ("" = not draining), with the
// exit-gate hold annotation appended while the gate is refusing.
func (d *drainer) Reason() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.reasonLocked()
}

// State is the structured drain status the socket server publishes: the reason
// and held flag are read together under the lock so they never disagree; seq is
// a monotone atomic read taken separately, which is fine — a follower watches
// it for change, not an exact value. Wired as the server's DrainFn.
func (d *drainer) State() socket.DrainState {
	d.mu.Lock()
	st := socket.DrainState{Reason: d.reasonLocked(), ExitHeld: d.holdDetail != ""}
	d.mu.Unlock()
	st.Seq = d.seq.Load()
	return st
}

// Exited reports whether the drainer called stop() to hand off to launchd.
func (d *drainer) Exited() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.exited
}

// observe is every slot's OnChange hook. It runs synchronously on FSM
// goroutines and must not block: command sends are non-blocking and the
// exit gate's file I/O is handed to a separate goroutine.
func (d *drainer) observe(st statemachine.Status) {
	if !st.Wedged {
		d.mu.Lock()
		draining := d.reason != ""
		d.mu.Unlock()
		if !draining {
			return // nothing wedged, no drain: stay off the hot status path
		}
	} else {
		d.Start("wedged guest: a VM survived force-stop (see the slot's cycle record)", "")
	}
	// A slot transition observed during a drain is real forward progress;
	// record it so a follower's stall timer resets (heartbeats alone do not).
	d.progress()
	d.recheck()
}

// recheck re-evaluates convergence, re-issuing pause+recycle to every
// non-converged slot, and kicks the exit attempt once converged. Called on
// every status change while draining, from requestReload/SIGHUP, and from
// the held-gate retry ticker. Bounded and cheap: sends are non-blocking,
// and a JOB slot always eventually transitions (max_job_duration),
// producing another status change.
func (d *drainer) recheck() {
	d.mu.Lock()
	reason := d.reason
	exited := d.exited
	d.mu.Unlock()
	if reason == "" || exited {
		return
	}
	converged := true
	for _, s := range d.slots {
		if stableStatus(s.Status()) {
			continue
		}
		converged = false
		// Re-issue BOTH: pause alone leaves a paused LISTENING slot with a
		// dropped recycle waiting out max-idle.
		s.Command(statemachine.Command{Kind: statemachine.CmdPause})
		s.Command(statemachine.Command{Kind: statemachine.CmdRecycle, Reason: "draining for " + reason})
	}
	if converged {
		go d.tryExit() // exit gate does file I/O; never on an FSM goroutine
	}
}

// tryExit re-verifies convergence (monotone, so cheap and safe), runs the
// exit gate, and either stops the daemon or holds with the gate's detail.
// Two stability passes bracket the exit gate: the first avoids running
// slow file I/O unnecessarily, the second guards against a Resume that
// landed during the gate's I/O and un-stabled a slot (issue #53). Between
// pass two and d.stop(), the window is a lock acquisition — not zero, but
// brief enough that any junk cycle from that residual cannot start a job
// before the daemon exits.
//
// needRecheck is set when the second stability pass forces a back-out. The
// defer clears exitRunning and then calls d.recheck() so the drain does
// not get stuck: if the slot stabilised between the return and the defer,
// its observe fired a new tryExit that returned immediately (exitRunning
// was still true); after the defer clears exitRunning the drain needs an
// explicit recheck to restart tryExit, because no further status change
// is guaranteed.
func (d *drainer) tryExit() {
	d.mu.Lock()
	if d.exited || d.exitRunning {
		d.mu.Unlock()
		return
	}
	d.exitRunning = true
	sha := d.acceptedSHA
	d.mu.Unlock()
	needRecheck := false
	defer func() {
		d.mu.Lock()
		d.exitRunning = false
		d.mu.Unlock()
		if needRecheck {
			d.recheck()
		}
	}()

	for _, s := range d.slots {
		if !stableStatus(s.Status()) {
			return // a later status change re-evaluates
		}
	}

	if d.exitGate != nil {
		if ok, detail := d.exitGate(sha); !ok {
			d.setHold(detail)
			d.log.Error("refusing to exit onto a config the respawn would refuse; fix the file (the drain stays converged; revalidates automatically)", "detail", detail)
			d.startRetry()
			return
		}
		// Gate accepted — clear any stale hold annotation now. If the second
		// stability pass backs out below, we must not leave a false "held"
		// status visible while waiting for the slot to re-converge.
		// The retry ticker stays alive until the exit is committed: if the
		// second pass backs out and CmdPause sends drop (full buffer), the
		// ticker keeps driving recheck() → tryExit() so the drain does not
		// stall waiting for the BACKOFF timer to fire organically.
		d.setHold("")
	}

	// Second stability pass: a Resume landing during the exit gate's file
	// I/O can un-stable a slot. Re-verify before committing the exit; the
	// next status change re-evaluates via observe → recheck → tryExit.
	for _, s := range d.slots {
		if !stableStatus(s.Status()) {
			needRecheck = true
			return
		}
	}

	d.mu.Lock()
	d.exited = true
	d.holdDetail = ""
	stopRetry := d.retryStop
	d.retryStop = nil
	reason := d.reason
	d.mu.Unlock()
	if stopRetry != nil {
		stopRetry()
	}
	d.log.Error("fleet idle; exiting for a cold start", "reason", reason)
	d.stop()
}

// startRetry ensures the held-exit-gate revalidation ticker is running, so
// fixing the file is sufficient — no RPC needed to unblock.
func (d *drainer) startRetry() {
	d.mu.Lock()
	if d.retryStop != nil || d.exited {
		d.mu.Unlock()
		return
	}
	interval := d.retryEvery
	if interval == 0 {
		interval = 30 * time.Second
	}
	done := make(chan struct{})
	var once sync.Once
	d.retryStop = func() { once.Do(func() { close(done) }) }
	d.mu.Unlock()
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				d.recheck()
			}
		}
	}()
}
