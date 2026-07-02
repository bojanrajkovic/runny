package images

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojanrajkovic/runny/internal/oci"
)

// testProto is the canonical test puller: stubbed-seam-ready, with short
// hold/poll budgets. Both this file's testAcquire and metrics_test.go's
// metricAcquire build on it so the two suites exercise identically-configured
// pullers.
func testProto(dir string) *imagePuller {
	return &imagePuller{
		destDir: dir, ref: oci.Ref{Host: "h", Name: "n", Tag: "t"},
		stall: time.Second, log: slog.Default(),
		holdBudget: 40 * time.Millisecond, pollInterval: 5 * time.Millisecond,
	}
}

// testAcquire subscribes to a puller for dir with stubbed seams (attempt /
// diskFree), exercising the registry + run-loop without a live registry or a
// real filesystem.
func testAcquire(t *testing.T, dir string, report func(string),
	attempt func(context.Context) (string, error),
	diskFree func(string) (uint64, error),
) (*subscription, func()) {
	t.Helper()
	proto := testProto(dir)
	proto.attempt = attempt
	proto.diskFree = diskFree
	return acquirePuller(dir, report, proto)
}

func recvWithin(t *testing.T, sub *subscription, within time.Duration) ensureResult {
	t.Helper()
	select {
	case r := <-sub.done:
		return r
	case <-time.After(within):
		t.Fatal("timed out waiting for puller result")
		return ensureResult{}
	}
}

func okFree(uint64) func(string) (uint64, error) {
	return func(string) (uint64, error) { return 1 << 62, nil }
}

// Concurrent subscribers to one dir share a single pull attempt and both
// receive its outcome — the core of issue #125.
func TestPullerSharesOutcomeAcrossSubscribers(t *testing.T) {
	dir := t.TempDir()
	var calls atomic.Int32
	release := make(chan struct{})
	attempt := func(ctx context.Context) (string, error) {
		calls.Add(1)
		<-release // hold the pull open so the second subscriber joins mid-flight
		return "sha256:shared", nil
	}
	subA, relA := testAcquire(t, dir, nil, attempt, okFree(0))
	defer relA()
	subB, relB := testAcquire(t, dir, nil, attempt, okFree(0))
	defer relB()

	close(release)
	rA := recvWithin(t, subA, time.Second)
	rB := recvWithin(t, subB, time.Second)
	if rA.err != nil || rB.err != nil {
		t.Fatalf("unexpected errors: A=%v B=%v", rA.err, rB.err)
	}
	if rA.digest != "sha256:shared" || rB.digest != "sha256:shared" {
		t.Fatalf("digests = %q / %q, want sha256:shared", rA.digest, rB.digest)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("attempt ran %d times, want exactly 1 (shared)", got)
	}
}

// A deterministic disk-headroom failure holds (polling free space) instead of
// re-pulling, and once headroom appears the next attempt succeeds — all
// subscribers advance, no FSM round-trip.
func TestPullerDiskHoldThenRecover(t *testing.T) {
	dir := t.TempDir()
	var attempts atomic.Int32
	attempt := func(ctx context.Context) (string, error) {
		if attempts.Add(1) == 1 {
			return "", &oci.DiskHeadroomError{Ref: "r", ImageBytes: 100, FreeBytes: 1}
		}
		return "sha256:recovered", nil
	}
	var polls atomic.Int32
	diskFree := func(string) (uint64, error) {
		// Short for the first poll, then plenty (triggers a retry).
		if polls.Add(1) <= 1 {
			return 1, nil
		}
		return 1 << 62, nil
	}
	sub, rel := testAcquire(t, dir, nil, attempt, diskFree)
	defer rel()

	r := recvWithin(t, sub, 2*time.Second)
	if r.err != nil || r.digest != "sha256:recovered" {
		t.Fatalf("got (%q, %v), want (sha256:recovered, nil)", r.digest, r.err)
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d, want 2 (one doomed, one after headroom)", attempts.Load())
	}
}

// A permanently-full disk gives up after the bounded hold window and broadcasts
// the typed disk error — no infinite loop.
func TestPullerDiskGiveUpBounded(t *testing.T) {
	dir := t.TempDir()
	attempt := func(ctx context.Context) (string, error) {
		return "", &oci.DiskHeadroomError{Ref: "r", ImageBytes: 100, FreeBytes: 1}
	}
	diskFree := func(string) (uint64, error) { return 1, nil } // never enough

	sub, rel := testAcquire(t, dir, nil, attempt, diskFree)
	defer rel()

	r := recvWithin(t, sub, 2*time.Second)
	var dh *oci.DiskHeadroomError
	if !errors.As(r.err, &dh) {
		t.Fatalf("give-up error = %v, want a wrapped *DiskHeadroomError", r.err)
	}
}

// A disk that oscillates at the headroom threshold — free space appears on the
// probe but the pull still fails (space eaten before PullTo's guard re-checks) —
// must NOT keep the puller alive forever. The hold budget is cumulative across
// re-attempts, so it gives up within the window instead of churning re-pulls.
// Without the cumulative deadline this test hangs (recvWithin times out).
func TestPullerDiskOscillationGivesUp(t *testing.T) {
	dir := t.TempDir()
	attempt := func(ctx context.Context) (string, error) {
		// Every attempt fails the guard, even though the probe below says ok.
		return "", &oci.DiskHeadroomError{Ref: "r", ImageBytes: 100, FreeBytes: 1}
	}
	diskFree := func(string) (uint64, error) { return 1 << 62, nil } // probe always says ok

	sub, rel := testAcquire(t, dir, nil, attempt, diskFree)
	defer rel()

	r := recvWithin(t, sub, 2*time.Second)
	var dh *oci.DiskHeadroomError
	if !errors.As(r.err, &dh) {
		t.Fatalf("oscillation give-up error = %v, want a wrapped *DiskHeadroomError", r.err)
	}
}

// A transient (non-deterministic) failure is broadcast to every subscriber so
// each slot's FSM owns the retry.
func TestPullerTransientPassthrough(t *testing.T) {
	dir := t.TempDir()
	boom := errors.New("registry 503")
	attempt := func(ctx context.Context) (string, error) { return "", boom }
	sub, rel := testAcquire(t, dir, nil, attempt, okFree(0))
	defer rel()

	r := recvWithin(t, sub, time.Second)
	if !errors.Is(r.err, boom) {
		t.Fatalf("error = %v, want the transient error", r.err)
	}
}

// When the last subscriber leaves, the puller's context is cancelled (so an
// in-flight pull is interrupted) and the registry entry is removed — promptly,
// without waiting out any budget.
func TestPullerLastOutStopsAndDeregisters(t *testing.T) {
	dir := t.TempDir()
	started := make(chan struct{})
	attempt := func(ctx context.Context) (string, error) {
		close(started)
		<-ctx.Done() // a long pull, interruptible by ctx
		return "", ctx.Err()
	}
	_, rel := testAcquire(t, dir, nil, attempt, okFree(0))
	<-started
	rel()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		pullerRegistryMu.Lock()
		_, present := pullerRegistry[dir]
		pullerRegistryMu.Unlock()
		if !present {
			return // deregistered
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("puller was not removed from the registry after the last subscriber left")
}

// One subscriber leaving does NOT kill a pull other subscribers still want.
func TestPullerRecycleOneKeepsPullAlive(t *testing.T) {
	dir := t.TempDir()
	proceed := make(chan struct{})
	attempt := func(ctx context.Context) (string, error) {
		select {
		case <-proceed:
			return "sha256:survivor", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	subA, relA := testAcquire(t, dir, nil, attempt, okFree(0))
	subB, relB := testAcquire(t, dir, nil, attempt, okFree(0))
	defer relB()

	relA()         // A bails while the pull is still open
	close(proceed) // let the pull finish
	_ = subA       // A may or may not have been delivered to; B must be
	r := recvWithin(t, subB, time.Second)
	if r.err != nil || r.digest != "sha256:survivor" {
		t.Fatalf("survivor got (%q, %v), want (sha256:survivor, nil)", r.digest, r.err)
	}
}

// A panic inside the pull attempt becomes a terminal error, never a hung
// subscriber (the silent-failure shape this project exists to kill).
func TestPullerPanicBroadcasts(t *testing.T) {
	dir := t.TempDir()
	attempt := func(ctx context.Context) (string, error) { panic("boom in the pull") }
	sub, rel := testAcquire(t, dir, nil, attempt, okFree(0))
	defer rel()

	r := recvWithin(t, sub, time.Second)
	if r.err == nil {
		t.Fatal("panicking attempt delivered a nil error, want a terminal error")
	}
}

// Stress the registry get-or-create vs last-out-delete seam under -race: a new
// subscriber must never attach to a stopped puller and hang.
func TestPullerRegistryRaceNoHang(t *testing.T) {
	dir := t.TempDir()
	attempt := func(ctx context.Context) (string, error) { return "sha256:fast", nil }
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sub, rel := testAcquire(t, dir, nil, attempt, okFree(0))
			select {
			case <-sub.done:
			case <-time.After(2 * time.Second):
				t.Error("subscriber hung — attached to a dead puller")
			}
			rel()
		}()
	}
	wg.Wait()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		pullerRegistryMu.Lock()
		_, present := pullerRegistry[dir]
		pullerRegistryMu.Unlock()
		if !present {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("registry still holds a puller after all subscribers left")
}

// A subscriber that joins racing the terminal broadcast must receive a result,
// never block forever.
func TestPullerSubscribeAtTerminalNeverHangs(t *testing.T) {
	for i := 0; i < 300; i++ {
		dir := t.TempDir()
		attempt := func(ctx context.Context) (string, error) { return "sha256:x", nil }
		// First subscriber starts the puller, which finishes ~immediately.
		subA, relA := testAcquire(t, dir, nil, attempt, okFree(0))
		// Second subscriber races the finish: either it joins in time and is
		// delivered to, or it arrives post-finish and starts a fresh puller.
		subB, relB := testAcquire(t, dir, nil, attempt, okFree(0))
		recvWithin(t, subA, time.Second)
		recvWithin(t, subB, time.Second)
		relA()
		relB()
	}
}
