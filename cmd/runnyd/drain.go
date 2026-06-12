package main

import (
	"log/slog"
	"sync"
	"time"

	"github.com/bojanrajkovic/runny/internal/statemachine"
)

// drainSlot is the slice of *statemachine.Slot the drainer needs, an
// interface so tests drive stubs on any OS.
type drainSlot interface {
	Command(statemachine.Command) bool
	Status() statemachine.Status
}

// drainer drives the fleet to stable idle and then hands the process to
// launchd for a cold start. Two causes share it (ADR-0012's wedge
// escalation and ADR-0014's config reload): pause holds each slot in
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

	mu          sync.Mutex // control-plane frequency; a mutex beats CAS choreography
	reason      string     // empty = not draining; feeds GetStatus `draining`
	holdDetail  string     // non-empty while the exit gate is refusing
	acceptedSHA string     // config sha256 recorded at Start (reload only)
	exited      bool
	exitRunning bool   // one tryExit at a time
	retryStop   func() // stops the exit-gate retry ticker
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

// Reason reports the active drain reason ("" = not draining), with the
// exit-gate hold annotation appended while the gate is refusing.
func (d *drainer) Reason() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.reason == "" {
		return ""
	}
	if d.holdDetail != "" {
		return d.reason + " — exit held: " + d.holdDetail
	}
	return d.reason
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
func (d *drainer) tryExit() {
	d.mu.Lock()
	if d.exited || d.exitRunning {
		d.mu.Unlock()
		return
	}
	d.exitRunning = true
	sha := d.acceptedSHA
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.exitRunning = false
		d.mu.Unlock()
	}()

	for _, s := range d.slots {
		if !stableStatus(s.Status()) {
			return // a later status change re-evaluates
		}
	}

	if d.exitGate != nil {
		if ok, detail := d.exitGate(sha); !ok {
			d.mu.Lock()
			d.holdDetail = detail
			d.mu.Unlock()
			d.log.Error("refusing to exit onto a config the respawn would refuse; fix the file (the drain stays converged; revalidates automatically)", "detail", detail)
			d.startRetry()
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
