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

// TestStopMachineForceStopHangBounded is R7: a force stop that never returns
// (a wedged hypervisor) must not hang teardown — the bounded ctx escapes it.
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

// TestStopMachineForceErrorRacesTerminalState is R8: force stop returns an
// error, but the guest reaches its terminal state moments later (the Error
// transition closes done on the watch goroutine). That must read as stopped,
// not a false "still running" wedge.
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
