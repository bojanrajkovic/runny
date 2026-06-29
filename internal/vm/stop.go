package vm

import (
	"context"
	"fmt"
	"time"

	"github.com/bojanrajkovic/runny/internal/bounded"
)

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

	// Force stop is a blocking cgo call; a wedged hypervisor would hang
	// teardown forever — the unbounded-operation failure this project exists
	// to kill. Run it off-goroutine and bound every wait. The buffered channel
	// lets the goroutine finish and exit even after we stop waiting (ctx
	// fired), so it never leaks.
	ferr := make(chan error, 1)
	go func() { ferr <- ops.forceStop() }()
	select {
	case err := <-ferr:
		if err != nil {
			// The terminal-state notice lands on the watch goroutine, so done
			// may not be closed at the instant forceStop reports failure (a
			// guest racing into its Error state). Give it a bounded grace
			// before declaring a still-running guest, rather than the
			// zero-grace check that mislabels that race a wedge.
			select {
			case <-done:
				return nil
			case <-time.After(stopSettle):
				return fmt.Errorf("force stop failed with guest still running: %w", err)
			case <-ctx.Done():
				return fmt.Errorf("guest stop: %w", context.Cause(ctx))
			}
		}
		select {
		case <-done:
			return nil
		case <-time.After(stopSettle):
			return fmt.Errorf("guest did not reach stopped state after force stop")
		case <-ctx.Done():
			return fmt.Errorf("guest stop: %w", context.Cause(ctx))
		}
	case <-done:
		return nil // terminal state reached before forceStop even returned
	case <-ctx.Done():
		return fmt.Errorf("guest stop: %w", context.Cause(ctx))
	}
}
