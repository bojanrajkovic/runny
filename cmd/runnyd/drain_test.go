package main

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojanrajkovic/runny/internal/statemachine"
)

// stubSlot is a drainSlot whose status the test controls. drop makes the
// first N Command calls return false (a full command buffer).
type stubSlot struct {
	mu   sync.Mutex
	st   statemachine.Status
	drop int
	cmds []statemachine.Command
}

func (s *stubSlot) Command(c statemachine.Command) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.drop > 0 {
		s.drop--
		return false
	}
	s.cmds = append(s.cmds, c)
	return true
}

func (s *stubSlot) Status() statemachine.Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st
}

func (s *stubSlot) setStatus(st statemachine.Status) {
	s.mu.Lock()
	s.st = st
	s.mu.Unlock()
}

func (s *stubSlot) commands() []statemachine.Command {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]statemachine.Command{}, s.cmds...)
}

var (
	stableSt = statemachine.Status{Paused: true, State: statemachine.StateBackoff}
	jobSt    = statemachine.Status{State: statemachine.StateJob}
	wedgedSt = statemachine.Status{Wedged: true, State: statemachine.StateTeardown}
)

// newTestDrainer wires a drainer over stubs with a counting stop.
func newTestDrainer(slots ...*stubSlot) (*drainer, *atomic.Int32, chan struct{}) {
	ds := make([]drainSlot, len(slots))
	for i, s := range slots {
		ds[i] = s
	}
	var stops atomic.Int32
	stopped := make(chan struct{})
	d := &drainer{
		slots: ds,
		log:   slog.Default(),
		stop: func() {
			if stops.Add(1) == 1 {
				close(stopped)
			}
		},
	}
	return d, &stops, stopped
}

func waitStopped(t *testing.T, stopped chan struct{}) {
	t.Helper()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("drainer did not stop in time")
	}
}

func assertNotStopped(t *testing.T, stops *atomic.Int32) {
	t.Helper()
	time.Sleep(50 * time.Millisecond) // let any stray tryExit goroutine run
	if n := stops.Load(); n != 0 {
		t.Fatalf("stop() called %d time(s), want 0", n)
	}
}

// waitFor polls cond until true or the deadline; the final report uses the
// caller's own assertion for a useful message.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
}

func TestDrainerAllIdleConvergesAndStopsOnce(t *testing.T) {
	a, b := &stubSlot{st: stableSt}, &stubSlot{st: stableSt}
	d, stops, stopped := newTestDrainer(a, b)
	if !d.Start("config reload (rpc): test", "sha") {
		t.Fatal("Start returned false on a fresh drainer")
	}
	waitStopped(t, stopped)
	time.Sleep(50 * time.Millisecond)
	if n := stops.Load(); n != 1 {
		t.Errorf("stop() called %d time(s), want exactly 1", n)
	}
	if !d.Exited() {
		t.Error("Exited() = false after stop")
	}
	// Further status changes after exit are no-ops.
	d.observe(stableSt)
	time.Sleep(50 * time.Millisecond)
	if n := stops.Load(); n != 1 {
		t.Errorf("stop() called %d time(s) after post-exit observe, want 1", n)
	}
}

func TestDrainerWaitsForJobSlot(t *testing.T) {
	busy := &stubSlot{st: jobSt}
	idle := &stubSlot{st: stableSt}
	d, stops, stopped := newTestDrainer(busy, idle)
	d.Start("config reload (rpc): test", "")
	assertNotStopped(t, stops)
	// The busy slot got pause+recycle (queued for after the job).
	if cmds := busy.commands(); len(cmds) < 2 {
		t.Errorf("busy slot received %d commands, want pause+recycle", len(cmds))
	}
	// The job finishes; the slot lands paused in BACKOFF and the status
	// change re-evaluates convergence.
	busy.setStatus(stableSt)
	d.observe(stableSt)
	waitStopped(t, stopped)
}

// A slot whose command buffer is full at drain start must still converge:
// the drainer re-issues on every status change instead of trusting the
// first send.
func TestDrainerReissuesDroppedCommands(t *testing.T) {
	flaky := &stubSlot{st: jobSt, drop: 4} // Start's pause+recycle and one observe's worth dropped
	d, _, stopped := newTestDrainer(flaky)
	d.Start("config reload (rpc): test", "")
	if got := flaky.commands(); len(got) != 0 {
		t.Fatalf("expected the first sends to drop, got %v", got)
	}
	// Status changes while still draining: re-issue until the buffer accepts.
	d.observe(jobSt)
	d.observe(jobSt)
	cmds := flaky.commands()
	if len(cmds) != 2 || cmds[0].Kind != statemachine.CmdPause || cmds[1].Kind != statemachine.CmdRecycle {
		t.Fatalf("re-issue did not land pause+recycle, got %v", cmds)
	}
	if want := "draining for config reload (rpc): test"; cmds[1].Reason != want {
		t.Errorf("recycle reason = %q, want %q", cmds[1].Reason, want)
	}
	flaky.setStatus(stableSt)
	d.observe(stableSt)
	waitStopped(t, stopped)
}

func TestDrainerStartFirstReasonWins(t *testing.T) {
	d, _, stopped := newTestDrainer(&stubSlot{st: jobSt})
	if !d.Start("wedged guest: a VM survived force-stop (see the slot's cycle record)", "") {
		t.Fatal("first Start lost")
	}
	if d.Start("config reload (rpc): test", "sha") {
		t.Error("second Start won over an active drain")
	}
	if got := d.Reason(); got != "wedged guest: a VM survived force-stop (see the slot's cycle record)" {
		t.Errorf("Reason() = %q, want the first cause", got)
	}
	select {
	case <-stopped:
		t.Fatal("stopped with a JOB slot outstanding")
	default:
	}
}

// A second accepted reload (or a reload accepted during a wedge drain) cannot
// re-Start, but its freshly-validated hash must still supersede the prior
// cause's acceptedSHA — otherwise the exit gate compares the on-disk file
// against a wedge's "" (or a stale earlier hash) and mis-warns.
func TestDrainerUpdateAcceptedSHASupersedes(t *testing.T) {
	var sawSHA atomic.Value
	d, _, stopped := newTestDrainer(&stubSlot{st: stableSt})
	d.exitGate = func(acceptedSHA string) (bool, string) {
		sawSHA.Store(acceptedSHA)
		return true, ""
	}
	d.Start("wedged guest: a VM survived force-stop (see the slot's cycle record)", "") // wedge: SHA ""
	d.UpdateAcceptedSHA("validated-reload-sha")                                         // reload accepted mid-drain
	d.recheck()
	waitStopped(t, stopped)
	if got, _ := sawSHA.Load().(string); got != "validated-reload-sha" {
		t.Errorf("exit gate saw acceptedSHA %q, want the superseding reload hash", got)
	}
	// Not draining → no-op (must not stash a hash that would mislead a later drain).
	d2, _, _ := newTestDrainer(&stubSlot{st: stableSt})
	d2.UpdateAcceptedSHA("ignored")
	if d2.acceptedSHA != "" {
		t.Errorf("UpdateAcceptedSHA set %q while not draining", d2.acceptedSHA)
	}
}

func TestDrainerWedgedSlotCountsAsConverged(t *testing.T) {
	wedged := &stubSlot{st: wedgedSt}
	idle := &stubSlot{st: stableSt}
	d, _, stopped := newTestDrainer(wedged, idle)
	// The wedge arrives as a status change, not an explicit Start.
	d.observe(wedgedSt)
	if d.Reason() == "" {
		t.Fatal("a wedged status did not start a drain")
	}
	waitStopped(t, stopped)
}

func TestDrainerObserveIsNoOpWhenIdle(t *testing.T) {
	s := &stubSlot{st: statemachine.Status{State: statemachine.StateListening}}
	d, stops, _ := newTestDrainer(s)
	d.observe(statemachine.Status{State: statemachine.StateListening})
	if len(s.commands()) != 0 {
		t.Errorf("observe issued commands with no drain active: %v", s.commands())
	}
	if n := stops.Load(); n != 0 {
		t.Errorf("observe stopped the daemon with no drain active")
	}
	if d.Reason() != "" {
		t.Errorf("Reason() = %q, want empty", d.Reason())
	}
}

// A failing exit gate HOLDS: no stop, the reason carries the hold
// annotation, and a recheck after the gate flips ok exits exactly once.
func TestDrainerExitGateHoldsThenReleases(t *testing.T) {
	var gateOK atomic.Bool
	var sawSHA atomic.Value
	d, stops, stopped := newTestDrainer(&stubSlot{st: stableSt})
	d.exitGate = func(acceptedSHA string) (bool, string) {
		sawSHA.Store(acceptedSHA)
		if gateOK.Load() {
			return true, ""
		}
		return false, "config.yaml no longer parses"
	}
	d.Start("config reload (rpc): test", "abc123")
	heldReason := "config reload (rpc): test — exit held: config.yaml no longer parses"
	waitFor(t, func() bool { return d.Reason() == heldReason })
	assertNotStopped(t, stops)
	if got := d.Reason(); got != heldReason {
		t.Errorf("held Reason() = %q", got)
	}
	if got, _ := sawSHA.Load().(string); got != "abc123" {
		t.Errorf("exit gate saw acceptedSHA %q, want abc123", got)
	}
	// The operator fixes the file; an explicit recheck (reload RPC path)
	// unblocks without waiting for the ticker.
	gateOK.Store(true)
	d.recheck()
	waitStopped(t, stopped)
	time.Sleep(50 * time.Millisecond)
	if n := stops.Load(); n != 1 {
		t.Errorf("stop() called %d time(s), want exactly 1", n)
	}
	if got := d.Reason(); got != "config reload (rpc): test" {
		t.Errorf("Reason() after release = %q, want the hold annotation gone", got)
	}
}

// The retry ticker alone (no RPC, no status change) re-runs the gate, so
// fixing the file is sufficient to unblock a held exit.
func TestDrainerExitGateRetriesOnTicker(t *testing.T) {
	var gateOK atomic.Bool
	d, _, stopped := newTestDrainer(&stubSlot{st: stableSt})
	d.retryEvery = 10 * time.Millisecond
	d.exitGate = func(string) (bool, string) {
		if gateOK.Load() {
			return true, ""
		}
		return false, "config.yaml no longer parses"
	}
	d.Start("config reload (rpc): test", "")
	time.Sleep(30 * time.Millisecond) // the hold is in place, ticker running
	gateOK.Store(true)
	waitStopped(t, stopped)
}

// drain_seq advances on a slot transition during a drain (and notifies), and
// State() reports the reason/held/seq as a unit. A frozen seq across the 30s
// heartbeat is what lets a follower distinguish a wedge from a healthy drain.
func TestDrainerProgressOnTransition(t *testing.T) {
	var notifies atomic.Int32
	d, _, _ := newTestDrainer(&stubSlot{st: jobSt})
	d.onProgress = func() { notifies.Add(1) }
	if got := d.State(); got.Seq != 0 || got.Reason != "" || got.ExitHeld {
		t.Fatalf("fresh State() = %+v, want zero value", got)
	}
	d.Start("config reload (rpc): test", "sha")
	before := d.State().Seq
	d.observe(jobSt) // a transition while draining
	after := d.State()
	if after.Seq <= before {
		t.Errorf("drain_seq did not advance on a transition: %d -> %d", before, after.Seq)
	}
	if notifies.Load() == 0 {
		t.Error("onProgress not called on a drain transition")
	}
	if after.Reason == "" {
		t.Error("State().Reason empty while draining")
	}
}

// An exit-gate hold flips drain_seq on both set and release (the hold change
// does not run through a slot's OnChange, so the drainer must bump it
// directly), and State().ExitHeld tracks it authoritatively.
func TestDrainerProgressAndStateOnHoldFlip(t *testing.T) {
	var gateOK atomic.Bool
	d, _, stopped := newTestDrainer(&stubSlot{st: stableSt})
	d.exitGate = func(string) (bool, string) {
		if gateOK.Load() {
			return true, ""
		}
		return false, "config.yaml no longer parses"
	}
	d.Start("config reload (rpc): test", "sha")
	waitFor(t, func() bool { return d.State().ExitHeld })
	held := d.State()
	if !held.ExitHeld {
		t.Fatal("State().ExitHeld false after the gate refused")
	}
	if held.Seq == 0 {
		t.Error("drain_seq did not advance on the hold being set")
	}
	gateOK.Store(true)
	d.recheck()
	waitStopped(t, stopped)
	final := d.State()
	if final.ExitHeld {
		t.Error("State().ExitHeld still set after release")
	}
	if final.Seq <= held.Seq {
		t.Errorf("drain_seq did not advance on hold release: %d -> %d", held.Seq, final.Seq)
	}
}

// A Resume that races with the exit gate can un-stable a slot while the
// gate's file I/O is in flight (issue #53). tryExit's second stability
// pass must catch this and defer the exit until the slot re-converges.
func TestTryExitSecondPassBlocksEarlyExit(t *testing.T) {
	slot := &stubSlot{st: stableSt}
	gateEntered := make(chan struct{})
	gateCanExit := make(chan struct{})
	d, stops, stopped := newTestDrainer(slot)
	// Gate is called twice: once by the first tryExit (blocked until
	// gateCanExit is closed), and once by the second tryExit after the slot
	// re-converges (passes through immediately since gateCanExit is already
	// closed — a closed channel select case always succeeds).
	d.exitGate = func(string) (bool, string) {
		select {
		case <-gateCanExit:
			return true, "" // second tryExit: gate already released
		default:
			close(gateEntered) // signal: first tryExit has entered the gate
			<-gateCanExit
			return true, ""
		}
	}
	d.Start("config reload (rpc): test", "")
	<-gateEntered
	// Simulate Resume landing while the gate's I/O is in flight.
	slot.setStatus(statemachine.Status{State: statemachine.StateBackoff}) // Paused: false
	close(gateCanExit)
	// Second stability pass must catch the unstable slot — daemon must not stop.
	assertNotStopped(t, stops)
	// Slot re-converges; the next observe drives tryExit to completion.
	slot.setStatus(stableSt)
	d.observe(stableSt)
	waitStopped(t, stopped)
}
