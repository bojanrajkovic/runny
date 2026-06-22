// Package home owns the runny home on-disk layout: every path runnyd touches
// lives under one root (deployment-resolved — see Dir), and this package is the
// only place that knows the shape. Nothing here is authoritative state — vms/ is
// swept at startup, images/ is a cache, cycles/ holds artifacts.
package home

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// Dir is the runny home root: every path runnyd touches lives under it. Its
// LOCATION is deployment-resolved, never overridden (no env/flag): a non-root
// system daemon uses SystemHomeDir; a per-user agent uses <run-user-home>/.runny
// from $HOME. Resolution is consistent across the daemon and its clients
// (ResolveServer / ResolveClient) so they can never disagree about where the
// socket and credentials live.
type Dir string

// SystemHomeDir is the fixed home of a non-root system daemon: its ENTIRE state
// (config, the App key, logs, images, vms, cycles, and the control socket) lives
// here, deliberately outside any user home so the service account needs no home
// directory of its own. The privileged installer creates it owned by the
// dedicated service account with an inheriting ACL granting the operator account
// directory write (to land the App key and atomically edit config), artifact
// reads, and socket access; the socket stays 0600 + that inherited ACL and is
// never world-accessible (opening it is full daemon control). No flag selects
// it — its presence (and ownership, for the daemon) does, over the per-user
// ~/.runny.
const SystemHomeDir = "/Library/Application Support/runny"

const socketName = "runnyd.sock"

// ResolveServer returns the home runnyd binds and writes: SystemHomeDir when it
// exists and is OWNED by this process's uid — the system-daemon deployment, run
// as the dedicated service account that owns the installer-created tree (a
// home-less account never needs $HOME) — else the per-user ~/.runny. Ownership,
// not writability, is the signal: the operator account holds an inheriting ACL
// granting it dir-write (to land the App key and atomically edit config), so a
// writability test would ALSO pass for an operator who runs runnyd by hand,
// letting a per-user daemon bind the system socket and stomp the real daemon's
// home. Only the owning account is the system daemon; everyone else falls back
// to their own ~/.runny.
func ResolveServer() (Dir, error) { return resolveServer(SystemHomeDir) }

func resolveServer(systemDir string) (Dir, error) {
	var st unix.Stat_t
	if err := unix.Stat(systemDir, &st); err == nil && st.Uid == uint32(os.Geteuid()) {
		return Dir(systemDir), nil
	}
	return resolvePerUser()
}

// ResolveClient returns the home runnyctl and other clients read and dial:
// SystemHomeDir when it EXISTS, else the per-user ~/.runny. Existence is the
// client signal — neither ownership (the operator reaches a system daemon's home
// through an inheriting ACL, never owning it) nor a connect probe: it selects
// WHICH home; the dial/read then reports a dead or unreadable one.
func ResolveClient() (Dir, error) { return resolveClient(SystemHomeDir) }

func resolveClient(systemDir string) (Dir, error) {
	if _, err := os.Stat(systemDir); err == nil {
		return Dir(systemDir), nil
	}
	return resolvePerUser()
}

// resolvePerUser is the per-user home, <run-user-home>/.runny from $HOME. It
// rejects an implausible $HOME — anything not an absolute path below root —
// rather than deriving a wrong home: launchd can hand a per-user agent a
// degenerate $HOME, and with no override to correct a bad derivation, that must
// fail loudly. The check is structural: filepath.Join Cleans, so "/", "//", and
// "///" all collapse to "/.runny", and a relative $HOME yields a relative home —
// IsAbs plus a non-root Clean rejects the whole class.
func resolvePerUser() (Dir, error) {
	h, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home dir: %w", err)
	}
	if !filepath.IsAbs(h) || filepath.Clean(h) == "/" {
		return "", fmt.Errorf("resolving home dir: implausible $HOME %q", h)
	}
	return Dir(filepath.Join(h, ".runny")), nil
}

func (d Dir) String() string { return string(d) }

func (d Dir) ConfigPath() string { return filepath.Join(string(d), "config.yaml") }
func (d Dir) SocketPath() string { return filepath.Join(string(d), socketName) }
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
		// Tighten dirs created by an older runny too (MkdirAll leaves
		// existing modes alone). The root at 0o700 is the boundary that
		// matters; the rest is defense in depth.
		if err := os.Chmod(p, 0o700); err != nil {
			return fmt.Errorf("tightening %s: %w", p, err)
		}
	}
	return nil
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
