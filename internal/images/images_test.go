package images

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojanrajkovic/runny/internal/bounded"
	"github.com/bojanrajkovic/runny/internal/home"
	"github.com/bojanrajkovic/runny/internal/obs"
	"github.com/bojanrajkovic/runny/internal/oci"
	"github.com/bojanrajkovic/runny/internal/tart"
)

// TestMain stubs prepareBundleDiskFn as a no-op for this package's whole test
// suite by default. prepareBundleDisk's real, windows-only VHDX conversion
// behavior is internal/vhdx's test suite's job -- this package's fakes
// (fakeBundle's placeholder disk.img, puller_test.go's in-memory-only fake
// attempt functions) were never meant to be real convertible images, so
// running the real implementation against them fails on windows for reasons
// unrelated to whatever each test actually checks. The two tests that cover
// prepareBundleDiskFn's call sites directly (below) override it locally.
func TestMain(m *testing.M) {
	prepareBundleDiskFn = func(tart.Bundle) error { return nil }
	os.Exit(m.Run())
}

// eventCapture collects obs events; the emitter can fire from the puller's
// goroutine, so appends are locked.
type eventCapture struct {
	mu     sync.Mutex
	events []obs.Event
}

func (c *eventCapture) emit(e obs.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
}

// all returns a snapshot of every captured event, in order.
func (c *eventCapture) all() []obs.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]obs.Event(nil), c.events...)
}

// actionEvents returns the captured events for one action name, in order.
func (c *eventCapture) actionEvents(name string) []obs.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []obs.Event
	for _, e := range c.events {
		if (e.Kind == obs.KindActionStarted || e.Kind == obs.KindActionEnded) && e.Action.Name == name {
			out = append(out, e)
		}
	}
	return out
}

// scopedCtx is a context carrying a live obs scope inside an ENSURE_IMAGE
// step, the shape the FSM hands Ensure.
func scopedCtx(c *eventCapture) context.Context {
	ctx := obs.WithCycle(context.Background(), c.emit, obs.CycleRef{Slot: "s-1", CycleID: "c1"})
	return obs.WithStep(ctx, "ENSURE_IMAGE")
}

// fakeBundle writes the three bundle files so tart.Bundle.Verify passes.
func fakeBundle(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range tart.BundleFiles {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// attrValue returns the value of key among an action event's attrs, "" if absent.
func attrValue(e obs.Event, key string) string {
	for _, a := range e.Action.Attrs {
		if a.Key == key {
			return a.Value
		}
	}
	return ""
}

// Ensure emits a resolve action and a wait-for-pull action carrying the pull
// id — the correlation handle to the shared pull actor — on both the started
// and ended events, with an honest ok outcome.
func TestEnsureEmitsResolveAndWaitForPull(t *testing.T) {
	cap := &eventCapture{}
	dir := t.TempDir()
	e := &Ensurer{
		Home: home.Dir(dir),
		Ref:  oci.Ref{Host: "h", Name: "n", Tag: "t"},
		resolve: func(ctx context.Context) (string, error) {
			return "sha256:abc", nil
		},
		acquire: func(destDir string, ref oci.Ref, report func(string)) (chan ensureResult, func()) {
			fakeBundle(t, destDir)
			sub := make(chan ensureResult, 1)
			sub <- ensureResult{digest: "sha256:abc", bundle: tart.Bundle(destDir)}
			return sub, func() {}
		},
	}

	digest, _, bundle, err := e.Ensure(scopedCtx(cap), nil, nil)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if digest != "sha256:abc" || bundle.Verify() != nil {
		t.Fatalf("Ensure returned (%q, %v), want the pulled bundle", digest, bundle)
	}

	res := cap.actionEvents(obs.ActionResolve)
	if len(res) != 2 || res[1].Action.Outcome != obs.OutcomeOK {
		t.Fatalf("resolve events = %+v, want started+ended ok", res)
	}
	wait := cap.actionEvents(obs.ActionWaitForPull)
	if len(wait) != 2 || wait[1].Action.Outcome != obs.OutcomeOK {
		t.Fatalf("wait-for-pull events = %+v, want started+ended ok", wait)
	}
	id0, id1 := attrValue(wait[0], obs.AttrPullID), attrValue(wait[1], obs.AttrPullID)
	if id0 == "" || id0 != id1 {
		t.Fatalf("pull id = (%q, %q), want the same non-empty id on both events", id0, id1)
	}
}

// A bundle already cached emits no wait-for-pull at all — absence in the
// trace means the sub-step didn't run; the cycle never touched the puller.
func TestEnsureCacheHitEmitsNoWaitForPull(t *testing.T) {
	cap := &eventCapture{}
	dir := t.TempDir()
	ref := oci.Ref{Host: "h", Name: "n", Tag: "t"}
	fakeBundle(t, home.Dir(dir).ImageBundleDir(ref.String(), "sha256:hit"))
	e := &Ensurer{
		Home:    home.Dir(dir),
		Ref:     ref,
		resolve: func(ctx context.Context) (string, error) { return "sha256:hit", nil },
		acquire: func(string, oci.Ref, func(string)) (chan ensureResult, func()) {
			t.Error("cache hit must not subscribe to the shared puller")
			return nil, nil
		},
	}

	digest, _, _, err := e.Ensure(scopedCtx(cap), nil, nil)
	if err != nil || digest != "sha256:hit" {
		t.Fatalf("Ensure = (%q, %v), want cache hit", digest, err)
	}
	if len(cap.actionEvents(obs.ActionResolve)) != 2 {
		t.Error("resolve action missing on the cache-hit path")
	}
	if got := cap.actionEvents(obs.ActionWaitForPull); len(got) != 0 {
		t.Errorf("cache hit emitted wait-for-pull events: %+v", got)
	}
}

// The cache-hit fast path must run prepareBundleDiskFn before declaring the
// bundle done -- a raw-only bundle (disk.img present, no disk.vhdx yet, e.g.
// a windows conversion interrupted by a crash before imagePuller.run ever
// got to it) reaches this path directly and never touches the puller at
// all, so this is the only place left that can convert it.
func TestEnsureCacheHitRunsPrepareBundleDisk(t *testing.T) {
	cap := &eventCapture{}
	dir := t.TempDir()
	ref := oci.Ref{Host: "h", Name: "n", Tag: "t"}
	fakeBundle(t, home.Dir(dir).ImageBundleDir(ref.String(), "sha256:hit"))

	var called atomic.Bool
	prev := prepareBundleDiskFn
	prepareBundleDiskFn = func(tart.Bundle) error {
		called.Store(true)
		return nil
	}
	t.Cleanup(func() { prepareBundleDiskFn = prev })

	e := &Ensurer{
		Home:    home.Dir(dir),
		Ref:     ref,
		resolve: func(ctx context.Context) (string, error) { return "sha256:hit", nil },
		acquire: func(string, oci.Ref, func(string)) (chan ensureResult, func()) {
			t.Error("cache hit must not subscribe to the shared puller")
			return nil, nil
		},
	}

	if _, _, _, err := e.Ensure(scopedCtx(cap), nil, nil); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !called.Load() {
		t.Error("cache-hit path did not run prepareBundleDiskFn")
	}
}

// A prepareBundleDiskFn failure on the cache-hit path must surface as an
// Ensure error -- silently returning the unconverted bundle would leave a
// windows caller to fail later at CloneVHDX with a far less obvious cause.
func TestEnsureCacheHitPrepareBundleDiskFailureSurfaces(t *testing.T) {
	cap := &eventCapture{}
	dir := t.TempDir()
	ref := oci.Ref{Host: "h", Name: "n", Tag: "t"}
	fakeBundle(t, home.Dir(dir).ImageBundleDir(ref.String(), "sha256:hit"))

	wantErr := errors.New("conversion failed")
	prev := prepareBundleDiskFn
	prepareBundleDiskFn = func(tart.Bundle) error { return wantErr }
	t.Cleanup(func() { prepareBundleDiskFn = prev })

	e := &Ensurer{
		Home:    home.Dir(dir),
		Ref:     ref,
		resolve: func(ctx context.Context) (string, error) { return "sha256:hit", nil },
		acquire: func(string, oci.Ref, func(string)) (chan ensureResult, func()) {
			t.Error("cache hit must not subscribe to the shared puller")
			return nil, nil
		},
	}

	_, _, _, err := e.Ensure(scopedCtx(cap), nil, nil)
	if !errors.Is(err, wantErr) {
		t.Fatalf("Ensure err = %v, want wrapping %v", err, wantErr)
	}
}

// A failed resolve ends the resolve action with outcome=error — the trace
// shows the registry round-trip as the failing sub-step.
func TestEnsureResolveFailureActionOutcome(t *testing.T) {
	cap := &eventCapture{}
	e := &Ensurer{
		Home:    home.Dir(t.TempDir()),
		Ref:     oci.Ref{Host: "h", Name: "n", Tag: "t"},
		resolve: func(ctx context.Context) (string, error) { return "", os.ErrDeadlineExceeded },
	}
	if _, _, _, err := e.Ensure(scopedCtx(cap), nil, nil); err == nil {
		t.Fatal("Ensure succeeded with a failing resolve")
	}
	res := cap.actionEvents(obs.ActionResolve)
	if len(res) != 2 || res[1].Action.Outcome != obs.OutcomeError {
		t.Fatalf("resolve events = %+v, want started+ended error", res)
	}
}

// tarballServer serves one fake runner tarball and returns its resolver.
func tarballServer(t *testing.T, status int) RunnerResolver {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte("tarball-bytes"))
	}))
	t.Cleanup(srv.Close)
	return func(bounded.Context) (string, string, string, error) {
		return "actions-runner-osx-arm64-9.9.9.tar.gz", srv.URL + "/runner.tar.gz", "", nil
	}
}

// newTarballEnsurer builds an *Ensurer whose RunnerCacheDir already exists
// (ensureRunnerTarball, unlike the old free function, resolves its
// destination through e.Home rather than taking a bare cacheDir param).
func newTarballEnsurer(t *testing.T, resolve RunnerResolver) *Ensurer {
	t.Helper()
	home := home.Dir(t.TempDir())
	if err := home.Ensure(); err != nil {
		t.Fatal(err)
	}
	return &Ensurer{Home: home, Runner: resolve, ResolveBudget: time.Second, StallBudget: time.Minute}
}

// ensureRunnerTarball brackets its whole body — resolve + download, or the
// cache hit — in a tarball-ensure action, so cache-hit cycles show an honest
// near-zero duration rather than nothing.
func TestEnsureRunnerTarballEmitsAction(t *testing.T) {
	cap := &eventCapture{}
	e := newTarballEnsurer(t, tarballServer(t, http.StatusOK))

	for i := 0; i < 2; i++ { // download, then cache hit — both emit the action
		_, asset, err := e.ensureRunnerTarball(scopedCtx(cap), nil)
		if err != nil || asset == "" {
			t.Fatalf("ensureRunnerTarball #%d: (%q, %v)", i, asset, err)
		}
	}
	got := cap.actionEvents(obs.ActionTarballEnsure)
	if len(got) != 4 {
		t.Fatalf("tarball-ensure emitted %d events, want 4 (started+ended twice)", len(got))
	}
	// Both ended events — the download (index 1) and the cache hit (index 3).
	for _, i := range []int{1, 3} {
		if e := got[i]; e.Kind != obs.KindActionEnded || e.Action.Outcome != obs.OutcomeOK {
			t.Errorf("tarball-ensure event %d = (%s, %q), want ended ok", i, e.Kind, e.Action.Outcome)
		}
	}
}

// The tarball GET itself is a classed round trip inside the tarball-ensure
// action: one tarball.download event on a real download, none on a cache
// hit (no request happens at all).
func TestEnsureRunnerTarballEmitsHTTPEvent(t *testing.T) {
	cap := &eventCapture{}
	e := newTarballEnsurer(t, tarballServer(t, http.StatusOK))

	for i := 0; i < 2; i++ { // download, then cache hit
		if _, _, err := e.ensureRunnerTarball(scopedCtx(cap), nil); err != nil {
			t.Fatalf("ensureRunnerTarball #%d: %v", i, err)
		}
	}

	var got []obs.HTTPEvent
	for _, e := range cap.all() {
		if e.Kind == obs.KindHTTP {
			got = append(got, *e.HTTP)
		}
	}
	if len(got) != 1 {
		t.Fatalf("got %d HTTP events %v, want exactly 1 (download only, no cache-hit request)", len(got), got)
	}
	if got[0].Class != obs.HTTPTarballDownload || got[0].Status != http.StatusOK || got[0].Method != http.MethodGet {
		t.Errorf("HTTP event = %+v, want GET %s 200", got[0], obs.HTTPTarballDownload)
	}
}

// KindTarballDone fires once per actual download — never on a cache hit —
// the same event/metric-in-lockstep rule TestEnsureRunnerTarballDownloadMetric
// checks for the metric side.
func TestEnsureRunnerTarballEmitsTarballDoneEvent(t *testing.T) {
	cap := &eventCapture{}
	e := newTarballEnsurer(t, tarballServer(t, http.StatusOK))

	for i := 0; i < 2; i++ { // download, then cache hit
		if _, _, err := e.ensureRunnerTarball(scopedCtx(cap), nil); err != nil {
			t.Fatalf("ensureRunnerTarball #%d: %v", i, err)
		}
	}

	var got []obs.Event
	for _, ev := range cap.all() {
		if ev.Kind == obs.KindTarballDone {
			got = append(got, ev)
		}
	}
	if len(got) != 1 {
		t.Fatalf("got %d KindTarballDone events, want exactly 1 (cache hit must not record)", len(got))
	}
	if got[0].Tarball.Outcome != obs.OutcomeOK {
		t.Errorf("KindTarballDone outcome = %q, want ok", got[0].Tarball.Outcome)
	}
}

// A download that fails on its own terms (not caller cancellation) still
// fires KindTarballDone, with outcome=error and the failure text.
func TestEnsureRunnerTarballFailureEmitsErrorOutcome(t *testing.T) {
	cap := &eventCapture{}
	e := newTarballEnsurer(t, tarballServer(t, http.StatusServiceUnavailable))

	if _, _, err := e.ensureRunnerTarball(scopedCtx(cap), nil); err == nil {
		t.Fatal("ensureRunnerTarball succeeded against a 503 download")
	}

	var got []obs.Event
	for _, ev := range cap.all() {
		if ev.Kind == obs.KindTarballDone {
			got = append(got, ev)
		}
	}
	if len(got) != 1 {
		t.Fatalf("got %d KindTarballDone events, want exactly 1", len(got))
	}
	if got[0].Tarball.Outcome != obs.OutcomeError || got[0].Tarball.Error == "" {
		t.Errorf("KindTarballDone = %+v, want outcome=error with a non-empty error text", got[0].Tarball)
	}
}

// A download truncated by the caller's own cancellation is not a download
// outcome — record nothing, the same rule a failed-on-its-own-terms download
// (above) is exempt from.
func TestEnsureRunnerTarballCancelledEmitsNoTarballDoneEvent(t *testing.T) {
	cap := &eventCapture{}
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

	e := newTarballEnsurer(t, resolve)
	if _, _, err := e.ensureRunnerTarball(obs.WithStep(obs.WithCycle(ctx, cap.emit, obs.CycleRef{Slot: "s-1", CycleID: "c1"}), "ENSURE_IMAGE"), nil); err == nil {
		t.Fatal("ensureRunnerTarball succeeded despite cancellation")
	}
	for _, ev := range cap.all() {
		if ev.Kind == obs.KindTarballDone {
			t.Fatalf("cancelled download emitted %+v, want nothing", ev)
		}
	}
}
