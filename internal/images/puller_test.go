package images

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojanrajkovic/runny/internal/obs"
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

// testPullerWithSeams is a testProto with its attempt/diskFree seams
// stubbed — the shared build step testAcquire and testAcquireWithEvents
// both start from, so the one extra field the latter sets (events) is the
// only difference between the two call sites.
func testPullerWithSeams(dir string,
	attempt func(context.Context) (string, error),
	diskFree func(string) (uint64, error),
) *imagePuller {
	proto := testProto(dir)
	proto.attempt = attempt
	proto.diskFree = diskFree
	return proto
}

// testAcquire subscribes to a puller for dir with stubbed seams (attempt /
// diskFree), exercising the registry + run-loop without a live registry or a
// real filesystem.
func testAcquire(t *testing.T, dir string, report func(string),
	attempt func(context.Context) (string, error),
	diskFree func(string) (uint64, error),
) (chan ensureResult, func()) {
	t.Helper()
	return acquirePuller(dir, report, testPullerWithSeams(dir, attempt, diskFree))
}

// testAcquireWithEvents is testAcquire plus a fake obs.Emitter wired onto
// the puller's own pull scope.
func testAcquireWithEvents(t *testing.T, dir string, emit obs.Emitter,
	attempt func(context.Context) (string, error),
	diskFree func(string) (uint64, error),
) (chan ensureResult, func()) {
	t.Helper()
	proto := testPullerWithSeams(dir, attempt, diskFree)
	proto.events = emit
	return acquirePuller(dir, nil, proto)
}

func recvWithin(t *testing.T, sub chan ensureResult, within time.Duration) ensureResult {
	t.Helper()
	select {
	case r := <-sub:
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
			case <-sub:
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

// A successful pull emits exactly one KindPullStarted and one
// KindPullFinished, both carrying the puller's own Pull identity (never a
// Cycle), with the finished payload reporting outcome=ok. eventCapture (see
// images_test.go) is the shared goroutine-safe obs.Emitter test double — the
// puller's own goroutine and the progress watcher's can both call in.
func TestPullerEmitsStartedAndFinishedWithPullIdentity(t *testing.T) {
	dir := t.TempDir()
	cap := &eventCapture{}
	attempt := func(ctx context.Context) (string, error) { return "sha256:x", nil }
	sub, rel := testAcquireWithEvents(t, dir, cap.emit, attempt, okFree(0))
	defer rel()
	recvWithin(t, sub, time.Second)

	var started, finished int
	for _, e := range cap.all() {
		if e.Pull == nil {
			t.Fatalf("event %s missing Pull identity: %+v", e.Kind, e)
		}
		switch e.Kind {
		case obs.KindPullStarted:
			started++
		case obs.KindPullFinished:
			finished++
			if e.PullInfo == nil || e.PullInfo.Outcome != obs.OutcomeOK {
				t.Errorf("KindPullFinished payload = %+v, want outcome=ok", e.PullInfo)
			}
		}
	}
	if started != 1 || finished != 1 {
		t.Fatalf("started=%d finished=%d, want exactly 1 each", started, finished)
	}
}

// Two concurrent subscribers to one shared pull produce exactly one
// KindPullStarted/KindPullFinished pair — issue #125's core guarantee.
// TestPullerSharesOutcomeAcrossSubscribers asserts the same sharing for the
// digest/subscriber-facing result; this is the event-level equivalent.
func TestPullerTwoSubscribersEmitExactlyOnePullFinished(t *testing.T) {
	dir := t.TempDir()
	cap := &eventCapture{}
	var calls atomic.Int32
	release := make(chan struct{})
	attempt := func(ctx context.Context) (string, error) {
		calls.Add(1)
		<-release // hold the pull open so the second subscriber joins mid-flight
		return "sha256:shared", nil
	}
	subA, relA := testAcquireWithEvents(t, dir, cap.emit, attempt, okFree(0))
	defer relA()
	subB, relB := testAcquire(t, dir, nil, attempt, okFree(0))
	defer relB()

	close(release)
	recvWithin(t, subA, time.Second)
	recvWithin(t, subB, time.Second)

	var started, finished int
	for _, e := range cap.all() {
		switch e.Kind {
		case obs.KindPullStarted:
			started++
		case obs.KindPullFinished:
			finished++
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("attempt ran %d times, want exactly 1 (shared)", calls.Load())
	}
	if started != 1 || finished != 1 {
		t.Fatalf("started=%d finished=%d across 2 subscribers, want exactly 1 each", started, finished)
	}
}

// The pull's HTTP traffic — a classed round trip through obs.HTTPTransport,
// the same transport oci.Client wires — carries the pull scope: the
// KindHTTP event lands with Pull set, proving realAttempt's traffic is no
// longer scope-less.
func TestPullerHTTPTrafficCarriesPullScope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("blob"))
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	cap := &eventCapture{}
	client := &http.Client{Transport: &obs.HTTPTransport{}}
	attempt := func(ctx context.Context) (string, error) {
		req, err := http.NewRequestWithContext(obs.WithHTTPClass(ctx, obs.HTTPRegistryBlob), http.MethodGet, srv.URL, nil)
		if err != nil {
			return "", err
		}
		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return "sha256:x", nil
	}
	sub, rel := testAcquireWithEvents(t, dir, cap.emit, attempt, okFree(0))
	defer rel()
	recvWithin(t, sub, time.Second)

	var httpEvents int
	for _, e := range cap.all() {
		if e.Kind != obs.KindHTTP {
			continue
		}
		httpEvents++
		if e.Pull == nil {
			t.Errorf("KindHTTP event missing Pull identity: %+v", e)
		}
		if e.HTTP.Class != obs.HTTPRegistryBlob {
			t.Errorf("HTTP class = %q, want %q", e.HTTP.Class, obs.HTTPRegistryBlob)
		}
	}
	if httpEvents != 1 {
		t.Fatalf("got %d KindHTTP events, want exactly 1", httpEvents)
	}
}

// The disk-hold retry status (p.report) lands as a KindDetail on the pull
// scope — runny.progress.last is visible against the pull, not just each
// subscribing cycle.
func TestPullerDiskHoldEmitsDetailUnderPullScope(t *testing.T) {
	dir := t.TempDir()
	cap := &eventCapture{}
	var attempts atomic.Int32
	attempt := func(ctx context.Context) (string, error) {
		if attempts.Add(1) == 1 {
			return "", &oci.DiskHeadroomError{Ref: "r", ImageBytes: 100, FreeBytes: 1}
		}
		return "sha256:recovered", nil
	}
	var polls atomic.Int32
	diskFree := func(string) (uint64, error) {
		if polls.Add(1) <= 1 {
			return 1, nil
		}
		return 1 << 62, nil
	}
	sub, rel := testAcquireWithEvents(t, dir, cap.emit, attempt, diskFree)
	defer rel()
	recvWithin(t, sub, 2*time.Second)

	var sawDetail bool
	for _, e := range cap.all() {
		if e.Kind == obs.KindDetail {
			sawDetail = true
			if e.Pull == nil {
				t.Errorf("KindDetail missing Pull identity: %+v", e)
			}
		}
	}
	if !sawDetail {
		t.Fatal("expected at least one KindDetail from the disk-hold retry status")
	}
}

// A panic inside the attempt still yields exactly one KindPullFinished — the
// same silent-failure-proofness TestPullerPanicBroadcasts checks for the
// subscriber-facing result.
func TestPullerEmitsFinishedOnPanic(t *testing.T) {
	dir := t.TempDir()
	cap := &eventCapture{}
	attempt := func(ctx context.Context) (string, error) { panic("boom in the pull") }
	sub, rel := testAcquireWithEvents(t, dir, cap.emit, attempt, okFree(0))
	defer rel()
	recvWithin(t, sub, time.Second)

	var finished int
	for _, e := range cap.all() {
		if e.Kind == obs.KindPullFinished {
			finished++
		}
	}
	if finished != 1 {
		t.Fatalf("KindPullFinished count = %d, want exactly 1", finished)
	}
}

// A pull that succeeds after its last subscriber has already left still
// finishes exactly once — the same race TestPullerLastOutStopsAndDeregisters
// exercises for the registry, here asserted for the KindPullFinished event.
func TestPullerEmitsFinishedEvenAfterLastSubscriberLeaves(t *testing.T) {
	dir := t.TempDir()
	cap := &eventCapture{}
	started := make(chan struct{})
	proceed := make(chan struct{})
	attempt := func(ctx context.Context) (string, error) {
		close(started)
		<-proceed // the pull already committed to disk; ignore cancellation
		return "sha256:landed", nil
	}
	_, rel := testAcquireWithEvents(t, dir, cap.emit, attempt, okFree(0))
	<-started
	rel()          // the only subscriber leaves mid-attempt
	close(proceed) // let the attempt land anyway

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var finished int
		for _, e := range cap.all() {
			if e.Kind == obs.KindPullFinished {
				finished++
			}
		}
		if finished == 1 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("KindPullFinished did not fire after the last subscriber left mid-attempt")
}

// A terminal failure (transient error handed back to the FSMs) emits exactly
// one KindPullFinished with outcome=error and the error text.
func TestPullerEmitsFinishedWithErrorOutcome(t *testing.T) {
	dir := t.TempDir()
	cap := &eventCapture{}
	boom := errors.New("registry 503")
	attempt := func(ctx context.Context) (string, error) { return "", boom }
	sub, rel := testAcquireWithEvents(t, dir, cap.emit, attempt, okFree(0))
	defer rel()
	recvWithin(t, sub, time.Second)

	var finished []obs.Event
	for _, e := range cap.all() {
		if e.Kind == obs.KindPullFinished {
			finished = append(finished, e)
		}
	}
	if len(finished) != 1 {
		t.Fatalf("KindPullFinished count = %d, want exactly 1", len(finished))
	}
	if finished[0].PullInfo == nil || finished[0].PullInfo.Outcome != obs.OutcomeError || finished[0].PullInfo.Error != boom.Error() {
		t.Fatalf("KindPullFinished payload = %+v, want outcome=error with the transient error text", finished[0].PullInfo)
	}
}

// A puller cancelled before any terminal outcome (last subscriber left,
// attempt itself returns the cancellation) emits no KindPullFinished at all —
// no fabricated outcome for a pull that never finished — but it does emit
// exactly one KindPullAbandoned, so a trace consumer's open root span still
// closes instead of leaking.
func TestPullerCancelledEmitsNoFinishedEvent(t *testing.T) {
	dir := t.TempDir()
	cap := &eventCapture{}
	started := make(chan struct{})
	attempt := func(ctx context.Context) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}
	_, rel := testAcquireWithEvents(t, dir, cap.emit, attempt, okFree(0))
	<-started
	rel()

	time.Sleep(50 * time.Millisecond) // let the puller goroutine wind down
	var abandoned int
	for _, e := range cap.all() {
		if e.Kind == obs.KindPullFinished {
			t.Fatalf("cancelled puller emitted %+v, want nothing", e)
		}
		if e.Kind == obs.KindPullAbandoned {
			abandoned++
		}
	}
	if abandoned != 1 {
		t.Fatalf("KindPullAbandoned count = %d, want exactly 1", abandoned)
	}
}

// A pull that gives up after the bounded disk-hold window emits
// KindPullFinished (finish() is called from inside holdForDisk itself), not
// KindPullAbandoned — the deadline-exceeded give-up is a real terminal
// outcome, not an abandonment.
func TestPullerDiskGiveUpEmitsFinishedNotAbandoned(t *testing.T) {
	dir := t.TempDir()
	cap := &eventCapture{}
	attempt := func(ctx context.Context) (string, error) {
		return "", &oci.DiskHeadroomError{Ref: "r", ImageBytes: 100, FreeBytes: 1}
	}
	diskFree := func(string) (uint64, error) { return 1, nil } // never enough
	sub, rel := testAcquireWithEvents(t, dir, cap.emit, attempt, diskFree)
	defer rel()
	recvWithin(t, sub, 2*time.Second)

	var finished, abandoned int
	for _, e := range cap.all() {
		switch e.Kind {
		case obs.KindPullFinished:
			finished++
		case obs.KindPullAbandoned:
			abandoned++
		}
	}
	if finished != 1 || abandoned != 0 {
		t.Fatalf("finished=%d abandoned=%d, want exactly 1 finished, 0 abandoned", finished, abandoned)
	}
}

// A pull cancelled WHILE holding for disk headroom (the last subscriber
// leaves mid-hold, before the deadline) is abandoned, not finished — the
// same no-fabricated-outcome rule as a cancellation mid-attempt.
func TestPullerCancelledDuringDiskHoldEmitsAbandoned(t *testing.T) {
	dir := t.TempDir()
	cap := &eventCapture{}
	holding := make(chan struct{})
	var holdOnce sync.Once
	attempt := func(ctx context.Context) (string, error) {
		return "", &oci.DiskHeadroomError{Ref: "r", ImageBytes: 100, FreeBytes: 1}
	}
	diskFree := func(string) (uint64, error) {
		holdOnce.Do(func() { close(holding) })
		return 1, nil // never enough — stays in the hold until cancelled
	}
	_, rel := testAcquireWithEvents(t, dir, cap.emit, attempt, diskFree)
	<-holding
	rel()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var finished, abandoned int
		for _, e := range cap.all() {
			switch e.Kind {
			case obs.KindPullFinished:
				finished++
			case obs.KindPullAbandoned:
				abandoned++
			}
		}
		if finished != 0 {
			t.Fatalf("cancelled-during-hold pull emitted KindPullFinished, want KindPullAbandoned only")
		}
		if abandoned == 1 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("cancellation during a disk hold did not emit exactly one KindPullAbandoned")
}
