package images

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/bojanrajkovic/runny/internal/bounded"
)

// pullCall is one captured PullDone invocation.
type pullCall struct {
	outcome string
	d       time.Duration
	bytes   int64
}

// pullRecorder is a fake Metrics that captures PullDone invocations.
type pullRecorder struct {
	mu    sync.Mutex
	calls []pullCall
}

func (r *pullRecorder) metrics() *Metrics {
	return &Metrics{PullDone: func(outcome string, d time.Duration, bytes int64) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.calls = append(r.calls, pullCall{outcome, d, bytes})
	}}
}

func (r *pullRecorder) snapshot() []pullCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

// metricAcquire is testAcquire with a metrics seam attached; attempt receives
// the proto so it can feed the byte counter the way realAttempt's progress
// callback does.
func metricAcquire(t *testing.T, dir string, m *Metrics,
	attempt func(p *imagePuller, ctx context.Context) (string, error),
) (*subscription, func()) {
	t.Helper()
	proto := testProto(dir)
	proto.metrics = m
	proto.diskFree = okFree(0)
	proto.attempt = func(ctx context.Context) (string, error) { return attempt(proto, ctx) }
	return acquirePuller(dir, nil, proto)
}

// One underlying pull with two subscribers records exactly one PullDone —
// outcome ok, a real duration, and the bytes the transfer fed.
func TestPullerRecordsPullDoneOncePerPull(t *testing.T) {
	rec := &pullRecorder{}
	dir := t.TempDir()
	release := make(chan struct{})
	attempt := func(p *imagePuller, ctx context.Context) (string, error) {
		p.pullBytes.Add(1234) // what realAttempt's progress callback does per chunk
		<-release
		return "sha256:shared", nil
	}
	subA, relA := metricAcquire(t, dir, rec.metrics(), attempt)
	defer relA()
	subB, relB := metricAcquire(t, dir, rec.metrics(), attempt)
	defer relB()
	close(release)
	recvWithin(t, subA, time.Second)
	recvWithin(t, subB, time.Second)

	calls := rec.snapshot()
	if len(calls) != 1 {
		t.Fatalf("PullDone fired %d times for one shared pull, want exactly 1", len(calls))
	}
	c := calls[0]
	if c.outcome != "ok" || c.bytes != 1234 || c.d <= 0 {
		t.Fatalf("PullDone(%q, %v, %d), want (ok, >0, 1234)", c.outcome, c.d, c.bytes)
	}
}

// A terminal failure (transient error handed back to the FSMs) records one
// PullDone with outcome=error.
func TestPullerRecordsPullDoneOnError(t *testing.T) {
	rec := &pullRecorder{}
	dir := t.TempDir()
	sub, rel := metricAcquire(t, dir, rec.metrics(), func(_ *imagePuller, ctx context.Context) (string, error) {
		return "", errors.New("registry 503")
	})
	defer rel()
	recvWithin(t, sub, time.Second)

	calls := rec.snapshot()
	if len(calls) != 1 || calls[0].outcome != "error" {
		t.Fatalf("PullDone calls = %+v, want one with outcome=error", calls)
	}
}

// A puller cancelled before any terminal outcome (last subscriber left)
// records nothing — no fabricated outcome for a pull that never finished.
func TestPullerCancelledRecordsNothing(t *testing.T) {
	rec := &pullRecorder{}
	dir := t.TempDir()
	started := make(chan struct{})
	_, rel := metricAcquire(t, dir, rec.metrics(), func(_ *imagePuller, ctx context.Context) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	})
	<-started
	rel()

	time.Sleep(50 * time.Millisecond) // let the puller goroutine wind down
	if calls := rec.snapshot(); len(calls) != 0 {
		t.Fatalf("cancelled puller recorded %+v, want nothing", calls)
	}
}

// The tarball download metric fires once per actual download — not on a
// cache hit — and reports a failed transfer as outcome=error.
func TestEnsureRunnerTarballDownloadMetric(t *testing.T) {
	var calls []string
	m := &Metrics{TarballDownloadDone: func(outcome string, d time.Duration) {
		calls = append(calls, outcome)
	}}
	dir := t.TempDir()
	resolve := tarballServer(t, http.StatusOK)

	for i := 0; i < 2; i++ { // download, then cache hit
		if _, _, err := EnsureRunnerTarball(context.Background(), dir, resolve, time.Second, time.Minute, nil, m, nil); err != nil {
			t.Fatalf("EnsureRunnerTarball #%d: %v", i, err)
		}
	}
	if len(calls) != 1 || calls[0] != "ok" {
		t.Fatalf("download metric calls = %v, want exactly [ok] (cache hit must not record)", calls)
	}

	failResolve := tarballServer(t, http.StatusServiceUnavailable)
	if _, _, err := EnsureRunnerTarball(context.Background(), t.TempDir(), failResolve, time.Second, time.Minute, nil, m, nil); err == nil {
		t.Fatal("EnsureRunnerTarball succeeded against a 503 download")
	}
	if len(calls) != 2 || calls[1] != "error" {
		t.Fatalf("download metric calls = %v, want [ok error]", calls)
	}
}

// A pull that completes successfully just as its last subscriber leaves must
// still record — the transfer really happened and the next cycle cache-hits
// it; skipping would undercount real pulls.
func TestPullerRecordsPullDoneWhenLastSubscriberLeavesAtSuccess(t *testing.T) {
	rec := &pullRecorder{}
	dir := t.TempDir()
	started := make(chan struct{})
	_, rel := metricAcquire(t, dir, rec.metrics(), func(_ *imagePuller, ctx context.Context) (string, error) {
		close(started)
		<-ctx.Done()                // the subscriber leaves mid-pull...
		return "sha256:landed", nil // ...but the transfer completes anyway
	})
	<-started // the attempt must be in flight before the subscriber bails
	rel()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if calls := rec.snapshot(); len(calls) == 1 {
			if calls[0].outcome != "ok" {
				t.Fatalf("outcome = %q, want ok for a completed pull", calls[0].outcome)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("completed pull recorded no PullDone after its last subscriber left")
}

// A download truncated by the caller's own cancellation (operator recycle,
// daemon shutdown) records nothing — the same rule as the pull side; only a
// download that failed on its own terms is an error outcome.
func TestEnsureRunnerTarballCancelledRecordsNothing(t *testing.T) {
	var calls []string
	m := &Metrics{TarballDownloadDone: func(outcome string, d time.Duration) {
		calls = append(calls, outcome)
	}}
	ctx, cancel := context.WithCancel(context.Background())
	blocked := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(blocked)
		<-r.Context().Done() // hold the body open until the client goes away
	}))
	t.Cleanup(srv.Close)
	resolve := func(bounded.Context) (string, string, string, error) {
		return "actions-runner-osx-arm64-9.9.9.tar.gz", srv.URL + "/runner.tar.gz", "", nil
	}
	go func() {
		<-blocked
		cancel()
	}()

	if _, _, err := EnsureRunnerTarball(ctx, t.TempDir(), resolve, time.Second, time.Minute, nil, m, nil); err == nil {
		t.Fatal("EnsureRunnerTarball succeeded despite cancellation")
	}
	if len(calls) != 0 {
		t.Fatalf("cancelled download recorded %v, want nothing", calls)
	}
}
