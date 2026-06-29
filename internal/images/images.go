// Package images implements the state machine's ImageEnsurer over the oci
// pull client and the home layout, plus the host-side actions-runner tarball
// cache that gets shared into guests.
package images

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bojanrajkovic/runny/internal/bounded"
	"github.com/bojanrajkovic/runny/internal/home"
	"github.com/bojanrajkovic/runny/internal/oci"
	"github.com/bojanrajkovic/runny/internal/tart"
	"github.com/bojanrajkovic/runny/internal/versioncore"
)

// defaultResolveTimeout is the fallback bound for the quick metadata
// round-trips (manifest resolve, runner-download resolve) that precede a
// stall-watched transfer; the configured value is Deadlines.Resolve.
const defaultResolveTimeout = 60 * time.Second

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
	// ResolveBudget bounds the metadata round-trips that precede the pull
	// (Deadlines.Resolve); zero takes the default.
	ResolveBudget time.Duration
	Log           *slog.Logger
}

func (e *Ensurer) resolveBudget() time.Duration {
	if e.ResolveBudget > 0 {
		return e.ResolveBudget
	}
	return defaultResolveTimeout
}

func (e *Ensurer) Ensure(ctx context.Context, report func(string), onDigestResolved func(string)) (digest, runnerVersion string, bundle tart.Bundle, err error) {
	// Runner tarball first: small, fails fast, and shared across slots
	// (per-file locking inside).
	if e.Runner != nil {
		_, runnerVersion, err = EnsureRunnerTarball(ctx, e.Home.RunnerCacheDir(), e.Runner, e.resolveBudget(), e.StallBudget, report, e.log())
		if err != nil {
			return "", "", "", fmt.Errorf("ensuring runner tarball: %w", err)
		}
	}

	client := oci.NewClient()
	// ENSURE_IMAGE deliberately runs with no state deadline (pull duration is
	// unknowable), so this quick metadata round-trip needs its own wall-clock
	// bound — without one, a registry that accepts TCP and goes silent hangs
	// the slot forever.
	rctx, rcancel := bounded.WithTimeout(ctx, e.resolveBudget())
	digest, err = client.Resolve(rctx, e.Ref)
	rcancel()
	if err != nil {
		return "", "", "", fmt.Errorf("resolving %s: %w", e.Ref, err)
	}
	if onDigestResolved != nil {
		onDigestResolved(digest)
	}
	dir := e.Home.ImageBundleDir(e.Ref.String(), digest)
	bundle = tart.Bundle(dir)
	if bundle.Verify() == nil {
		return digest, runnerVersion, bundle, nil // cache hit
	}

	e.log().Info("pulling image", "ref", e.Ref.String(), "digest", digest)
	// Delegate the byte-pull to the shared per-dir puller: concurrent slots of a
	// pool enter ENSURE_IMAGE together, and this lets them share one in-flight
	// pull AND its outcome — including a bounded, shared wait when the pull is
	// deterministically doomed (disk headroom), instead of each slot re-running
	// the doomed pull and churning its own backoff (issue #125). Resolve and the
	// cache check stay per-slot above; the puller is keyed by the content-
	// addressed dir, so every subscriber necessarily wants this exact digest.
	pinned := e.Ref
	pinned.Digest = digest // pull exactly what we resolved
	sub, release := acquireImagePull(dir, pinned, e.StallBudget, e.log(), report)
	defer release()
	// The subscriber has no stall watch of its own: its liveness is the puller's
	// contract — the puller always reaches a terminal finish (a per-attempt stall
	// watch bounds each pull, diskHoldBudget bounds the disk hold, and a panic is
	// converted to a terminal error) — or this ctx is cancelled.
	select {
	case res := <-sub.done:
		if res.err != nil {
			return "", "", "", res.err
		}
		if err := res.bundle.Verify(); err != nil {
			return "", "", "", fmt.Errorf("pulled image incomplete: %w", err)
		}
		e.log().Info("image cached", "digest", digest)
		return digest, runnerVersion, res.bundle, nil
	case <-ctx.Done():
		// Operator recycle or daemon shutdown: leave the shared pull running for
		// any sibling still waiting (release drops only this subscription).
		return "", "", "", ctx.Err()
	}
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
	detail := fmt.Sprintf("pulled %s at %s/s", oci.HumanBytes(total), oci.HumanBytes(int64(rate)))
	p.lastDetail = detail
	logIt := now.Sub(p.lastLog) >= 15*time.Second
	if logIt {
		p.lastLog = now
	}
	p.mu.Unlock()

	p.report(detail)
	if logIt {
		p.log.Info("pull progress", "downloaded", oci.HumanBytes(total), "rate", oci.HumanBytes(int64(rate))+"/s")
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

func (e *Ensurer) log() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.Default()
}

// RunnerResolver yields the service-blessed runner build (filename, url,
// and the service-declared sha256, possibly empty on older GHES) — see
// github.Client.RunnerDownload for why "latest release" is not it. It takes
// a bounded.Context because it hits the GitHub API from a state that
// carries no deadline of its own — the bound is mandatory, enforced by the
// type.
type RunnerResolver func(ctx bounded.Context) (filename, url, sha256 string, err error)

// tarballLocks serializes per-filename ensures: slots of the same OS race to
// the same file; one downloads, the rest wait and find it cached. A
// capacity-1 channel rather than a mutex so the wait is ctx-aware and
// visible — the same fix the per-destination image-pull lock got: a mutex
// wait here was uninterruptible by operator recycle AND invisible (no
// watcher is armed yet, so the slot just sat with no annotation at all).
// The wait is transitively bounded by the holder's own stall-watched
// download (a wait on a peer's already-bounded operation is itself bounded).
var tarballLocks sync.Map // filename -> chan struct{} (capacity-1 semaphore)

// supersededTarballs returns the full paths of cached runner tarballs that
// EnsureRunnerTarball should drop when staging assetName: same-flavor builds
// (the actions-runner-<os>-arm64 prefix) that are STRICTLY OLDER than
// assetName. Guests stage their cycle's exact tarball by name (see
// guest.provisionScript), so this is disk GC, not correctness — but it still
// must not delete a tarball a concurrent slot is using. It deliberately never
// returns a newer-or-equal sibling: a concurrent slot resolving a NEWER version
// shares this prefix but holds a different per-assetName lock and may have just
// renamed its tarball in; deleting it would pull that slot's tarball out from
// under its guest. The residual it does NOT close — dropping an OLDER sibling a
// slow concurrent slot is still staging — is a rare rollover race whose worst
// case is one loud exit-78 cycle that crash-only recycles, not a silent
// mismatch. assetName is GitHub's filename verbatim, so the shape is guarded
// (an unguarded [:4] would panic the whole daemon on a renamed asset, not fail
// one cycle); an unparseable version on either side is left alone rather than
// guessed at. .partial temps belong to whichever slot is mid-download and are
// skipped.
func supersededTarballs(cacheDir, assetName string) []string {
	parts := strings.Split(assetName, "-")
	if len(parts) < 4 {
		return nil
	}
	prefix := strings.Join(parts[:4], "-") // actions-runner-<os>-arm64
	myVer := versioncore.Core(strings.TrimPrefix(assetName, prefix+"-"))
	if myVer == "" {
		return nil // can't tell what's older than a version we can't parse
	}
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return nil
	}
	var stale []string
	for _, e := range entries {
		name := e.Name()
		if name == assetName || !strings.HasPrefix(name, prefix) ||
			strings.HasSuffix(name, ".partial") {
			continue
		}
		theirVer := versioncore.Core(strings.TrimPrefix(name, prefix+"-"))
		if theirVer == "" || versioncore.Compare(theirVer, myVer) >= 0 {
			continue // unparseable, or newer-or-equal: keep it
		}
		stale = append(stale, filepath.Join(cacheDir, name))
	}
	return stale
}

// EnsureRunnerTarball makes sure the service-current actions-runner tarball
// sits in cacheDir (the virtiofs share). Returns the tarball path and the
// asset filename (the version identifier, e.g. "actions-runner-osx-arm64-2.320.0.tar.gz").
// The download is stall-watched and progress-reported — no unbounded silent
// network reads anywhere (a startup-time version of this dead-stalled on a
// hung GitHub download with no timeout). Superseded same-OS tarballs are
// removed so guests (which pick by name) never stage a deprecated build.
func EnsureRunnerTarball(ctx context.Context, cacheDir string, resolve RunnerResolver, resolveBudget, stallBudget time.Duration, report func(string), log *slog.Logger) (string, string, error) {
	if resolveBudget <= 0 {
		resolveBudget = defaultResolveTimeout
	}
	rctx, rcancel := bounded.WithTimeout(ctx, resolveBudget)
	assetName, assetURL, wantSHA, err := resolve(rctx)
	rcancel()
	if err != nil {
		return "", "", err
	}

	semAny, _ := tarballLocks.LoadOrStore(assetName, make(chan struct{}, 1))
	sem := semAny.(chan struct{})
	select {
	case sem <- struct{}{}:
	default:
		if report != nil {
			report("waiting for a concurrent download of " + assetName)
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return "", "", fmt.Errorf("waiting for a concurrent download of %s: %w", assetName, context.Cause(ctx))
		}
	}
	defer func() { <-sem }()

	dest := filepath.Join(cacheDir, assetName)
	if _, err := os.Stat(dest); err == nil {
		return dest, assetName, nil // already cached
	}
	for _, p := range supersededTarballs(cacheDir, assetName) {
		_ = os.Remove(p)
	}

	if log != nil {
		log.Info("downloading runner tarball", "asset", assetName)
	}
	if report != nil {
		report("downloading " + assetName)
	}
	stall := bounded.NewStall()
	wctx, cancel := stall.Watch(ctx, stallBudget)
	defer cancel()

	dreq, err := http.NewRequestWithContext(wctx, http.MethodGet, assetURL, nil)
	if err != nil {
		return "", "", err
	}
	dresp, err := http.DefaultClient.Do(dreq)
	if err != nil {
		return "", "", stallErr(wctx, err, "downloading runner tarball")
	}
	defer dresp.Body.Close()
	if dresp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("downloading runner tarball: HTTP %d", dresp.StatusCode)
	}
	tmp := dest + ".partial"
	f, err := os.Create(tmp)
	if err != nil {
		return "", "", err
	}
	prog := newProgress(report, log, stallBudget)
	body := io.TeeReader(dresp.Body, progressWriter(func(n int64) {
		stall.Feed(n)
		prog.feed(n)
	}))
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), body)
	prog.stop()
	if err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return "", "", stallErr(wctx, err, "downloading runner tarball")
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", "", err
	}
	// The tarball is staged into every guest and executed; verify it against
	// the service-declared checksum when one was given (older GHES may omit
	// it — a missing checksum downgrades to the TLS-only trust we had).
	if wantSHA != "" {
		if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, wantSHA) {
			_ = os.Remove(tmp)
			return "", "", fmt.Errorf("runner tarball checksum mismatch: downloads endpoint says %s, got %s", wantSHA, got)
		}
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return "", "", err
	}
	if log != nil {
		log.Info("runner tarball cached", "asset", assetName)
	}
	return dest, assetName, nil
}

// stallErr surfaces the stall cause when the watcher killed a transfer —
// "transfer stalled: no progress for 3m" beats a bare "context canceled".
func stallErr(wctx context.Context, err error, doing string) error {
	if cause := context.Cause(wctx); cause != nil && wctx.Err() != nil {
		return fmt.Errorf("%s: %w", doing, cause)
	}
	return fmt.Errorf("%s: %w", doing, err)
}

// progressWriter adapts a byte-delta callback to io.Writer for TeeReader.
type progressWriter func(int64)

func (p progressWriter) Write(b []byte) (int, error) {
	p(int64(len(b)))
	return len(b), nil
}
