package vm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bojanrajkovic/runny/internal/bounded"
)

// fakeStopOps is a scriptable stopOps: requestStop/forceStop return canned
// values and record whether they were called, and forceStop can block until
// released (to simulate a hypervisor that never returns).
type fakeStopOps struct {
	reqOK       bool
	reqErr      error
	reqCalled   bool
	forceErr    error
	forceCalled bool
	forceBlock  <-chan struct{} // if non-nil, forceStop blocks on it before returning
}

func (f *fakeStopOps) requestStop() (bool, error) {
	f.reqCalled = true
	return f.reqOK, f.reqErr
}

func (f *fakeStopOps) forceStop() error {
	f.forceCalled = true
	if f.forceBlock != nil {
		<-f.forceBlock
	}
	return f.forceErr
}

func shortSettle(t *testing.T, d time.Duration) {
	t.Helper()
	prev := stopSettle
	stopSettle = d
	t.Cleanup(func() { stopSettle = prev })
}

// run invokes stopMachine in a goroutine and fails if it does not return within
// timeout — so a regression that lets the force stop hang shows up as a test
// failure, not a hung suite.
func run(t *testing.T, ctx bounded.Context, grace time.Duration, done <-chan struct{}, ops stopOps, timeout time.Duration) error {
	t.Helper()
	res := make(chan error, 1)
	go func() { res <- stopMachine(ctx, grace, done, ops) }()
	select {
	case err := <-res:
		return err
	case <-time.After(timeout):
		t.Fatalf("stopMachine did not return within %v (it hung)", timeout)
		return nil
	}
}

func bg(t *testing.T, d time.Duration) bounded.Context {
	t.Helper()
	ctx, cancel := bounded.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// TestStopMachineForceStopHangBounded: a force stop that never returns (a
// wedged hypervisor) must not hang teardown — the bounded ctx escapes it.
func TestStopMachineForceStopHangBounded(t *testing.T) {
	block := make(chan struct{})
	t.Cleanup(func() { close(block) }) // release the goroutine at test end
	ops := &fakeStopOps{forceBlock: block}
	done := make(chan struct{}) // guest never reaches terminal state

	err := run(t, bg(t, 50*time.Millisecond), time.Millisecond, done, ops, 2*time.Second)
	if err == nil || !strings.Contains(err.Error(), "guest stop") {
		t.Fatalf("hung force stop: got err=%v, want a bounded \"guest stop\" error", err)
	}
}

// TestStopMachineForceErrorRacesTerminalState: force stop returns an error, but
// the guest reaches its terminal state moments later (the Error transition
// closes done on the watch goroutine). That must read as stopped, not a false
// "still running" wedge.
func TestStopMachineForceErrorRacesTerminalState(t *testing.T) {
	shortSettle(t, time.Second)
	ops := &fakeStopOps{forceErr: errors.New("stop boom")}
	done := make(chan struct{})
	go func() {
		time.Sleep(20 * time.Millisecond) // terminal-state notice lands shortly after
		close(done)
	}()

	if err := run(t, bg(t, 5*time.Second), time.Millisecond, done, ops, 3*time.Second); err != nil {
		t.Fatalf("guest reached terminal state within grace: got err=%v, want nil", err)
	}
}

// TestStopMachineForceErrorStillRunning: force stop errors and the guest never
// stops — we must still surface the failure (bounded by settle), never hang.
func TestStopMachineForceErrorStillRunning(t *testing.T) {
	shortSettle(t, 100*time.Millisecond)
	ops := &fakeStopOps{forceErr: errors.New("stop boom")}
	done := make(chan struct{}) // never closes

	err := run(t, bg(t, 5*time.Second), time.Millisecond, done, ops, 2*time.Second)
	if err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("genuinely wedged guest: got err=%v, want a \"still running\" error", err)
	}
}

// TestStopMachineForceCleanNoTerminalState: force stop returns clean but the
// guest never reaches a terminal state (watchState never closes done). The
// nil-error settle arm must surface a bounded "did not reach stopped state"
// error rather than hang or report success.
func TestStopMachineForceCleanNoTerminalState(t *testing.T) {
	shortSettle(t, 100*time.Millisecond)
	ops := &fakeStopOps{}       // forceStop returns nil
	done := make(chan struct{}) // never closes

	err := run(t, bg(t, 5*time.Second), time.Millisecond, done, ops, 2*time.Second)
	if err == nil || !strings.Contains(err.Error(), "did not reach stopped state") {
		t.Fatalf("clean force but no terminal state: got err=%v, want a bounded \"did not reach stopped state\" error", err)
	}
}

// TestStopMachineGraceful: a graceful RequestStop that the guest honors within
// grace returns nil without ever force-stopping.
func TestStopMachineGraceful(t *testing.T) {
	ops := &fakeStopOps{reqOK: true}
	done := make(chan struct{})
	go func() {
		time.Sleep(10 * time.Millisecond)
		close(done)
	}()

	if err := run(t, bg(t, 5*time.Second), time.Second, done, ops, 3*time.Second); err != nil {
		t.Fatalf("graceful stop: got err=%v, want nil", err)
	}
	if ops.forceCalled {
		t.Fatal("graceful stop honored within grace must not force-stop")
	}
}

// TestStopMachineAlreadyStopped: a done already closed short-circuits before
// touching either op.
func TestStopMachineAlreadyStopped(t *testing.T) {
	ops := &fakeStopOps{}
	done := make(chan struct{})
	close(done)

	if err := run(t, bg(t, time.Second), time.Second, done, ops, time.Second); err != nil {
		t.Fatalf("already stopped: got err=%v, want nil", err)
	}
	if ops.reqCalled || ops.forceCalled {
		t.Fatal("already-stopped guest must not call requestStop/forceStop")
	}
}

// raceStopOps closes done from inside a stop primitive, so the terminal state
// lands mid-sequence rather than before stopMachine's own already-stopped
// early-out — which is what makes the racing select reachable at all.
type raceStopOps struct {
	done     chan struct{}
	closeOn  string // "request" or "force"
	block    chan struct{}
	forceRet error
}

func (o *raceStopOps) requestStop() (bool, error) {
	if o.closeOn == "request" {
		close(o.done)
	}
	return false, nil // decline, so the sequence goes straight to force
}

func (o *raceStopOps) forceStop() error {
	if o.closeOn == "force" {
		close(o.done)
		return o.forceRet
	}
	<-o.block // never returns: the pre-force select is the one under test
	return o.forceRet
}

// A guest that reaches its terminal state at the same instant the deadline
// expires has STOPPED — the stop succeeded, and reporting a deadline error for
// it is a false wedge. That matters: a wedged stop is what makes the daemon
// restart cold once idle, so the same physical outcome must not report success
// or catastrophe depending on which ready case Go's uniform select happens to
// pick. Run it enough times that a coin flip cannot pass.
func TestStopMachineTerminalStateWinsOverSimultaneousDeadline(t *testing.T) {
	shortSettle(t, time.Second)
	block := make(chan struct{})
	t.Cleanup(func() { close(block) }) // release the parked forceStop goroutines
	for i := range 200 {
		ctx, cancel := bounded.WithTimeout(context.Background(), time.Nanosecond)
		<-ctx.Done()
		ops := &raceStopOps{done: make(chan struct{}), closeOn: "request", block: block}
		err := stopMachine(ctx, time.Millisecond, ops.done, ops)
		cancel()
		if err != nil {
			t.Fatalf("iteration %d: guest was stopped, got err=%v, want nil (false wedge)", i, err)
		}
	}
}

// Deliberately not tested: the same tie in the post-force select. Reaching it
// requires forceStop to have already run, and if it has not, `done` is open and
// a deadline error is the CORRECT answer — so the assertion would only ever
// hold by timing luck. Both selects resolve the tie through stoppedOrDeadline,
// which the test above pins.
