//go:build windows

package vm

import (
	"context"
	"testing"

	"github.com/bojanrajkovic/runny/internal/obs"
)

// forceStop runs as the floor after the grace wait, which is precisely when
// the Stop call's own ctx has often already expired. Its window therefore
// has to satisfy two things at once, and they pull in opposite directions:
// a deadline the caller's expiry cannot poison, and the caller's values so
// the Terminate still reports into the cycle that forced it. Building it
// from a bare context.Background() satisfies only the first, and the
// resulting spans surface as disconnected trace roots rather than inside
// that cycle.
func TestForceStopCtxKeepsScopeAfterCallerExpired(t *testing.T) {
	scoped := obs.WithCycle(context.Background(), func(obs.Event) {}, obs.CycleRef{
		Slot: "slot-1",
		Pool: "pool",
	})
	expired, cancel := context.WithCancel(scoped)
	cancel()

	ctx, release := hcsStopOps{ctx: expired}.forceStopCtx()
	defer release()

	if err := ctx.Err(); err != nil {
		t.Fatalf("force-stop window inherited the caller's expiry: %v", err)
	}
	if !obs.Live(ctx) {
		t.Fatal("force-stop window dropped the observability scope; its spans will orphan")
	}
}
