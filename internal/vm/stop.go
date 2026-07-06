package vm

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bojanrajkovic/runny/internal/bounded"
)

// runAsync launches a blocking call — a cgo machine operation that can wedge —
// and returns its result on a buffered channel so the caller can bound the wait
// with a select while the call runs. The buffer means the goroutine always
// completes its send and exits once the call returns, even if the caller
// already stopped waiting. A call that NEVER returns (a truly wedged
// hypervisor) keeps that one goroutine parked in cgo until the process
// restarts — unavoidable for an uncancellable cgo call, and acceptable because
// the caller's wait stays bounded, which is the point.
func runAsync(fn func() error) <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- fn() }()
	return ch
}

// stopOps are a platform machine's two stop primitives, split from the
// sequencing so the bounded stop logic is testable off-darwin — the real ops
// are blocking cgo calls into Virtualization.framework (vzMachine in
// vz_darwin.go). requestStop asks the guest to power off gracefully (often
// ignored by vanilla images); forceStop is the hard kill.
type stopOps interface {
	requestStop() (bool, error)
	forceStop() error
}

// stopSettle bounds how long stopMachine waits for the terminal-state
// notification to land on the watch goroutine after the force stop resolves —
// both after a clean force stop and after one that errored while the guest was
// racing into its Error state. Overridable in tests.
var stopSettle = 10 * time.Second

// stopMachine runs the bounded stop sequence every platform shares: an
// optional graceful RequestStop bounded by grace, then a force stop. done
// closes when the machine reaches a terminal state (Stopped or Error). Force
// is the floor; this only errors if even force-stop failed (or hung) with the
// guest still running.
func stopMachine(ctx bounded.Context, grace time.Duration, done <-chan struct{}, ops stopOps) error {
	select {
	case <-done:
		return nil // already stopped
	default:
	}

	if ok, err := ops.requestStop(); ok && err == nil {
		select {
		case <-done:
			return nil
		case <-time.After(grace):
		case <-ctx.Done():
		}
	}

	// Force stop is a blocking cgo call; running it through runAsync bounds the
	// wait so a wedged hypervisor can't hang teardown forever — the
	// unbounded-operation failure this project exists to kill.
	force := runAsync(ops.forceStop)
	select {
	case err := <-force:
		// The terminal-state notice lands on the watch goroutine, so done may
		// not be closed at the instant forceStop resolves — whether it errored
		// (a guest racing into its Error state) or returned clean (Stopped
		// notice still in flight). Grace done before declaring a wedge, rather
		// than a zero-grace check that mislabels that race. The two outcomes
		// differ only in the message they carry if the grace expires.
		wedged := errors.New("guest did not reach stopped state after force stop")
		if err != nil {
			wedged = fmt.Errorf("force stop failed with guest still running: %w", err)
		}
		select {
		case <-done:
			return nil
		case <-time.After(stopSettle):
			return wedged
		case <-ctx.Done():
			return stopDeadlineErr(ctx, err)
		}
	case <-done:
		return nil // terminal state reached before forceStop returned
	case <-ctx.Done():
		return stopDeadlineErr(ctx, nil)
	}
}

// stopDeadlineErr renders the teardown-deadline error, folding in a force-stop
// failure when one occurred so a wedged hypervisor's own error is never lost to
// the deadline race — failure is never silent.
func stopDeadlineErr(ctx bounded.Context, forceErr error) error {
	if forceErr != nil {
		return fmt.Errorf("guest stop: %w (force stop also failed: %v)", context.Cause(ctx), forceErr)
	}
	return fmt.Errorf("guest stop: %w", context.Cause(ctx))
}
