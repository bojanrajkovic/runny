// Package home owns the ~/.runny on-disk layout: every path runnyd touches
// lives under one root, and this package is the only place that knows the
// shape. Nothing here is authoritative state — vms/ is swept at startup,
// images/ is a cache, cycles/ holds artifacts (ADR-0004).
package home

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Dir is the runny home root (default ~/.runny, overridable via --home or
// RUNNY_HOME).
type Dir string

// Resolve picks the home root: explicit flag > RUNNY_HOME > ~/.runny.
func Resolve(flag string) (Dir, error) {
	switch {
	case flag != "":
		return Dir(flag), nil
	case os.Getenv("RUNNY_HOME") != "":
		return Dir(os.Getenv("RUNNY_HOME")), nil
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home dir: %w", err)
	}
	return Dir(filepath.Join(h, ".runny")), nil
}

func (d Dir) String() string { return string(d) }

func (d Dir) ConfigPath() string { return filepath.Join(string(d), "config.yaml") }
func (d Dir) SocketPath() string { return filepath.Join(string(d), "runnyd.sock") }
func (d Dir) LockPath() string   { return filepath.Join(string(d), "runnyd.lock") }
func (d Dir) LogsDir() string    { return filepath.Join(string(d), "logs") }
func (d Dir) LogFile() string    { return filepath.Join(d.LogsDir(), "runnyd.log") }
func (d Dir) ImagesDir() string  { return filepath.Join(string(d), "images") }
func (d Dir) VMsDir() string     { return filepath.Join(string(d), "vms") }
func (d Dir) CyclesDir() string  { return filepath.Join(string(d), "cycles") }

// RunnerCacheDir holds actions-runner tarballs, shared read-only into guests
// via virtiofs so the download happens once per version, not once per cycle.
func (d Dir) RunnerCacheDir() string {
	return filepath.Join(string(d), "cache", "actions-runner")
}

// ImageBundleDir is the cache location for one pulled image, keyed by
// registry reference and digest: images/<sanitized-ref>/sha256-<hex>/.
func (d Dir) ImageBundleDir(ref, digest string) string {
	return filepath.Join(d.ImagesDir(), sanitizeRef(ref), strings.ReplaceAll(digest, ":", "-"))
}

// VMDir is the ephemeral clone bundle for one slot. Always deletable.
func (d Dir) VMDir(slot string) string { return filepath.Join(d.VMsDir(), slot) }

// SlotCyclesDir holds the per-cycle artifact dirs for one slot.
func (d Dir) SlotCyclesDir(slot string) string { return filepath.Join(d.CyclesDir(), slot) }

// Ensure creates the directory skeleton owner-only throughout: the tree
// holds credentials-adjacent material end to end — App key path in config,
// the control socket, and post-mortem runner _diag logs that can contain
// unmasked job secrets. Everything under it is read by the daemon's own
// user only (RunnyBar and runnyctl run as the same user; virtiofs shares
// are exported by the daemon process itself).
func (d Dir) Ensure() error {
	for _, p := range []string{
		string(d), d.LogsDir(), d.ImagesDir(), d.VMsDir(), d.CyclesDir(), d.RunnerCacheDir(),
	} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			return fmt.Errorf("creating %s: %w", p, err)
		}
	}
	// Tighten a tree created by an older runny (MkdirAll leaves existing
	// directories' modes alone).
	return os.Chmod(string(d), 0o700)
}

// sanitizeRef makes an OCI reference filesystem-safe:
// ghcr.io/cirruslabs/macos-tahoe-xcode:26.3 -> ghcr.io_cirruslabs_macos-tahoe-xcode.
// The tag is dropped — the digest component of the bundle path is the identity.
func sanitizeRef(ref string) string {
	if i := strings.LastIndexByte(ref, ':'); i > strings.LastIndexByte(ref, '/') {
		ref = ref[:i]
	}
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		ref = ref[:i]
	}
	return strings.ReplaceAll(ref, "/", "_")
}
