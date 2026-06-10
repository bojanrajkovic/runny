// Package images implements the state machine's ImageEnsurer over the oci
// pull client and the home layout, plus the host-side actions-runner tarball
// cache that gets shared into guests.
package images

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bojanrajkovic/runny/internal/home"
	"github.com/bojanrajkovic/runny/internal/oci"
	"github.com/bojanrajkovic/runny/internal/tart"
)

// Ensurer resolves and caches the configured image (ENSURE_IMAGE's work).
type Ensurer struct {
	Home home.Dir
	Ref  oci.Ref
	// Runner resolves the service-blessed actions-runner build for this
	// pool's guest OS. Ensuring the tarball happens here, inside the cycle's
	// ENSURE_IMAGE state, NOT at daemon startup: a download must always have
	// stall detection, retry-with-backoff, and why-visibility — a startup
	// prime had none and dead-stalled exactly the way sand used to.
	Runner RunnerResolver
	// StallBudget: the progress-based deadline — no layer bytes for this long
	// means stuck (slow ≠ silent).
	StallBudget time.Duration
	Log         *slog.Logger
}

func (e *Ensurer) Ensure(ctx context.Context, report func(string)) (string, tart.Bundle, error) {
	// Runner tarball first: small, fails fast, and shared across slots
	// (per-file locking inside).
	if e.Runner != nil {
		if _, err := EnsureRunnerTarball(ctx, e.Home.RunnerCacheDir(), e.Runner, e.StallBudget, report, e.log()); err != nil {
			return "", "", fmt.Errorf("ensuring runner tarball: %w", err)
		}
	}

	client := oci.NewClient()
	digest, err := client.Resolve(ctx, e.Ref)
	if err != nil {
		return "", "", fmt.Errorf("resolving %s: %w", e.Ref, err)
	}
	dir := e.Home.ImageBundleDir(e.Ref.String(), digest)
	bundle := tart.Bundle(dir)
	if bundle.Verify() == nil {
		return digest, bundle, nil // cache hit
	}

	e.log().Info("pulling image", "ref", e.Ref.String(), "digest", digest)
	stall := oci.NewStall()
	prog := newProgress(report, e.log(), e.StallBudget)
	client.Progress = func(n int64) {
		stall.Feed(n)
		prog.feed(n)
	}
	defer prog.stop()
	wctx, cancel := stall.Watch(ctx, e.StallBudget)
	defer cancel()
	pinned := e.Ref
	pinned.Digest = digest // pull exactly what we resolved
	if _, err := client.PullTo(wctx, pinned, dir); err != nil {
		if cause := context.Cause(wctx); cause != nil && wctx.Err() != nil {
			return "", "", fmt.Errorf("pulling %s: %w", e.Ref, cause)
		}
		return "", "", fmt.Errorf("pulling %s: %w", e.Ref, err)
	}
	if err := bundle.Verify(); err != nil {
		return "", "", fmt.Errorf("pulled image incomplete: %w", err)
	}
	e.log().Info("image cached", "digest", digest)
	return digest, bundle, nil
}

// progress turns raw byte deltas into operator-visible pull progress: a
// live status annotation (throttled to 1/s) and a log line every 15s. Slow
// and stuck must look DIFFERENT — when no data arrives, the annotation says
// so explicitly instead of freezing on the last healthy reading (a wedged
// download once sat behind "pulled 25.2 MiB at 4.2 MiB/s" for minutes).
type progress struct {
	report func(string)
	log    *slog.Logger
	budget time.Duration

	mu         sync.Mutex
	total      int64
	winBytes   int64
	winStart   time.Time
	lastShow   time.Time
	lastLog    time.Time
	lastFeed   time.Time
	lastDetail string
	done       chan struct{}
	exited     chan struct{}
	stopOnce   sync.Once
}

func newProgress(report func(string), log *slog.Logger, stallBudget time.Duration) *progress {
	now := time.Now()
	p := &progress{
		report: report, log: log, budget: stallBudget,
		winStart: now, lastLog: now, lastFeed: now,
		done:   make(chan struct{}),
		exited: make(chan struct{}),
	}
	if report != nil {
		go p.watchStaleness()
	} else {
		close(p.exited)
	}
	return p
}

// watchStaleness annotates the report when data stops arriving, counting up
// toward the stall budget so the operator can see the kill coming.
func (p *progress) watchStaleness() {
	defer close(p.exited)
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-t.C:
			p.mu.Lock()
			idle := time.Since(p.lastFeed)
			detail := p.lastDetail
			p.mu.Unlock()
			if idle >= 10*time.Second {
				stalled := fmt.Sprintf("STALLED %s (no data; teardown at %s) — last: %s",
					idle.Round(time.Second), p.budget, detail)
				p.report(stalled)
			}
		}
	}
}

func (p *progress) feed(n int64) {
	if p.report == nil {
		return
	}
	p.mu.Lock()
	p.total += n
	p.winBytes += n
	now := time.Now()
	p.lastFeed = now
	if now.Sub(p.lastShow) < time.Second {
		p.mu.Unlock()
		return
	}
	rate := float64(p.winBytes) / max(now.Sub(p.winStart).Seconds(), 0.001)
	total := p.total
	p.winBytes, p.winStart, p.lastShow = 0, now, now
	detail := fmt.Sprintf("pulled %s at %s/s", humanBytes(total), humanBytes(int64(rate)))
	p.lastDetail = detail
	logIt := now.Sub(p.lastLog) >= 15*time.Second
	if logIt {
		p.lastLog = now
	}
	p.mu.Unlock()

	p.report(detail)
	if logIt {
		p.log.Info("pull progress", "downloaded", humanBytes(total), "rate", humanBytes(int64(rate))+"/s")
	}
}

func (p *progress) stop() {
	p.stopOnce.Do(func() { close(p.done) })
	// Join the watcher before clearing: select picks randomly among ready
	// cases, so an un-joined goroutine could emit a stale "STALLED" report
	// after the FSM has moved on and cleared the status detail.
	<-p.exited
	if p.report != nil {
		p.report("")
	}
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func (e *Ensurer) log() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.Default()
}

// RunnerResolver yields the service-blessed runner build (filename, url) —
// see github.Client.RunnerDownload for why "latest release" is not it.
type RunnerResolver func(ctx context.Context) (filename, url string, err error)

// tarballLocks serializes per-filename ensures: slots of the same OS race to
// the same file; one downloads, the rest wait and find it cached.
var tarballLocks sync.Map // filename -> *sync.Mutex

// EnsureRunnerTarball makes sure the service-current actions-runner tarball
// sits in cacheDir (the virtiofs share). Returns the tarball path. The
// download is stall-watched and progress-reported — no unbounded silent
// network reads anywhere (a startup-time version of this dead-stalled on a
// hung GitHub download with no timeout). Superseded same-OS tarballs are
// removed so guests (which pick by name) never stage a deprecated build.
func EnsureRunnerTarball(ctx context.Context, cacheDir string, resolve RunnerResolver, stallBudget time.Duration, report func(string), log *slog.Logger) (string, error) {
	assetName, assetURL, err := resolve(ctx)
	if err != nil {
		return "", err
	}

	muAny, _ := tarballLocks.LoadOrStore(assetName, &sync.Mutex{})
	mu := muAny.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()

	dest := filepath.Join(cacheDir, assetName)
	if _, err := os.Stat(dest); err == nil {
		return dest, nil // already cached
	}
	// Drop superseded tarballs of the same flavor (osx vs linux prefix).
	// assetName is GitHub's filename verbatim, so guard the shape — an
	// unguarded [:4] would panic the whole daemon on a renamed asset, not
	// fail one cycle. Skip .partial temps: they belong to whichever slot is
	// mid-download (locks are per-assetName, so a sibling downloading a
	// NEWER version shares this prefix but not this lock).
	if parts := strings.Split(assetName, "-"); len(parts) >= 4 {
		prefix := strings.Join(parts[:4], "-") // actions-runner-<os>-arm64
		if entries, err := os.ReadDir(cacheDir); err == nil {
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), prefix) && e.Name() != assetName &&
					!strings.HasSuffix(e.Name(), ".partial") {
					_ = os.Remove(filepath.Join(cacheDir, e.Name()))
				}
			}
		}
	}

	if log != nil {
		log.Info("downloading runner tarball", "asset", assetName)
	}
	if report != nil {
		report("downloading " + assetName)
	}
	stall := oci.NewStall()
	wctx, cancel := stall.Watch(ctx, stallBudget)
	defer cancel()

	dreq, err := http.NewRequestWithContext(wctx, http.MethodGet, assetURL, nil)
	if err != nil {
		return "", err
	}
	dresp, err := http.DefaultClient.Do(dreq)
	if err != nil {
		return "", tarballErr(wctx, err)
	}
	defer dresp.Body.Close()
	if dresp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("downloading runner tarball: HTTP %d", dresp.StatusCode)
	}
	tmp := dest + ".partial"
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	prog := newProgress(report, log, stallBudget)
	body := io.TeeReader(dresp.Body, progressWriter(func(n int64) {
		stall.Feed(n)
		prog.feed(n)
	}))
	_, err = io.Copy(f, body)
	prog.stop()
	if err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return "", tarballErr(wctx, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if log != nil {
		log.Info("runner tarball cached", "asset", assetName)
	}
	return dest, nil
}

// tarballErr surfaces the stall cause when the watcher killed the download.
func tarballErr(wctx context.Context, err error) error {
	if cause := context.Cause(wctx); cause != nil && wctx.Err() != nil {
		return fmt.Errorf("downloading runner tarball: %w", cause)
	}
	return fmt.Errorf("downloading runner tarball: %w", err)
}

// progressWriter adapts a byte-delta callback to io.Writer for TeeReader.
type progressWriter func(int64)

func (p progressWriter) Write(b []byte) (int, error) {
	p(int64(len(b)))
	return len(b), nil
}
