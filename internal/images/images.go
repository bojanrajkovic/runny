// Package images implements the state machine's ImageEnsurer over the oci
// pull client and the home layout, plus the host-side actions-runner tarball
// cache that gets shared into guests.
package images

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
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
	// StallBudget: the progress-based deadline — no layer bytes for this long
	// means stuck (slow ≠ silent).
	StallBudget time.Duration
	Log         *slog.Logger
}

func (e *Ensurer) Ensure(ctx context.Context, report func(string)) (string, tart.Bundle, error) {
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
	prog := newProgress(report, e.log())
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
// ghcr and stuck pulls must look different (the predecessor's biggest
// diagnosability gap).
type progress struct {
	report func(string)
	log    *slog.Logger

	mu        sync.Mutex
	total     int64
	winBytes  int64
	winStart  time.Time
	lastShow  time.Time
	lastLog   time.Time
	startedAt time.Time
}

func newProgress(report func(string), log *slog.Logger) *progress {
	now := time.Now()
	return &progress{report: report, log: log, winStart: now, lastLog: now, startedAt: now}
}

func (p *progress) feed(n int64) {
	if p.report == nil {
		return
	}
	p.mu.Lock()
	p.total += n
	p.winBytes += n
	now := time.Now()
	if now.Sub(p.lastShow) < time.Second {
		p.mu.Unlock()
		return
	}
	rate := float64(p.winBytes) / max(now.Sub(p.winStart).Seconds(), 0.001)
	total := p.total
	p.winBytes, p.winStart, p.lastShow = 0, now, now
	logIt := now.Sub(p.lastLog) >= 15*time.Second
	if logIt {
		p.lastLog = now
	}
	p.mu.Unlock()

	detail := fmt.Sprintf("pulled %s at %s/s", humanBytes(total), humanBytes(int64(rate)))
	p.report(detail)
	if logIt {
		p.log.Info("pull progress", "downloaded", humanBytes(total), "rate", humanBytes(int64(rate))+"/s")
	}
}

func (p *progress) stop() {
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

// EnsureRunnerTarball makes sure the latest actions-runner osx-arm64 tarball
// sits in cacheDir (the virtiofs share). Returns the tarball path. Existing
// versions are kept; guests pick the newest by name sort.
func EnsureRunnerTarball(ctx context.Context, cacheDir string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.github.com/repos/actions/runner/releases/latest", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetching latest runner release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetching latest runner release: HTTP %d", resp.StatusCode)
	}
	var rel struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", err
	}
	var assetURL, assetName string
	for _, a := range rel.Assets {
		if matched, _ := filepath.Match("actions-runner-osx-arm64-*.tar.gz", a.Name); matched {
			assetURL, assetName = a.BrowserDownloadURL, a.Name
			break
		}
	}
	if assetURL == "" {
		return "", fmt.Errorf("release %s has no osx-arm64 runner asset", rel.TagName)
	}
	dest := filepath.Join(cacheDir, assetName)
	if _, err := os.Stat(dest); err == nil {
		return dest, nil // already cached
	}

	dreq, err := http.NewRequestWithContext(ctx, http.MethodGet, assetURL, nil)
	if err != nil {
		return "", err
	}
	dresp, err := http.DefaultClient.Do(dreq)
	if err != nil {
		return "", fmt.Errorf("downloading runner tarball: %w", err)
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
	if _, err := io.Copy(f, dresp.Body); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return "", fmt.Errorf("downloading runner tarball: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return "", err
	}
	return dest, nil
}
