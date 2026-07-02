package images

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bojanrajkovic/runny/internal/bounded"
	"github.com/bojanrajkovic/runny/internal/home"
	"github.com/bojanrajkovic/runny/internal/obs"
	"github.com/bojanrajkovic/runny/internal/oci"
	"github.com/bojanrajkovic/runny/internal/tart"
)

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
		acquire: func(destDir string, ref oci.Ref, report func(string)) (*subscription, func()) {
			fakeBundle(t, destDir)
			sub := &subscription{done: make(chan ensureResult, 1)}
			sub.done <- ensureResult{digest: "sha256:abc", bundle: tart.Bundle(destDir)}
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
		acquire: func(string, oci.Ref, func(string)) (*subscription, func()) {
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

// EnsureRunnerTarball brackets its whole body — resolve + download, or the
// cache hit — in a tarball-ensure action, so cache-hit cycles show an honest
// near-zero duration rather than nothing.
func TestEnsureRunnerTarballEmitsAction(t *testing.T) {
	cap := &eventCapture{}
	dir := t.TempDir()
	resolve := tarballServer(t, http.StatusOK)

	for i := 0; i < 2; i++ { // download, then cache hit — both emit the action
		_, asset, err := EnsureRunnerTarball(scopedCtx(cap), dir, resolve, time.Second, time.Minute, nil, nil, nil)
		if err != nil || asset == "" {
			t.Fatalf("EnsureRunnerTarball #%d: (%q, %v)", i, asset, err)
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
