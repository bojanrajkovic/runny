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
)

// Dir is the runny home root: every path runnyd touches lives under it. Its
// LOCATION is deployment-resolved, never overridden (no env/flag): a non-root
// system daemon uses SystemHomeDir; a per-user agent uses <run-user-home>/.runny
// from $HOME. Resolution is consistent across the daemon and its clients
// (ResolveServer / ResolveClient) so they can never disagree about where the
// socket and credentials live.
type Dir string

// SystemHomeDir is declared per platform (home_unix.go / home_windows.go),
// alongside ownedByCurrentUser, the ownership probe resolveServer keys on.

const socketName = "runnyd.sock"

// ResolveServer returns the home runnyd binds and writes: SystemHomeDir when it
// exists and is OWNED by this process's uid — the system-daemon deployment, run
// as the dedicated service account that owns the installer-created tree (a
// home-less account never needs $HOME) — else the per-user ~/.runny. Ownership,
// not writability, is the signal: the operator account holds an ACL entry
// granting it dir-write (to land the App key and reach the socket), so a
// writability test would ALSO pass for an operator who runs runnyd by hand,
// letting a per-user daemon bind the system socket and stomp the real daemon's
// home. Only the owning account is the system daemon; everyone else falls back
// to their own ~/.runny.
func ResolveServer() (Dir, error) { return resolveServer(SystemHomeDir) }

func resolveServer(systemDir string) (Dir, error) {
	if ownedByCurrentUser(systemDir) {
		return Dir(systemDir), nil
	}
	return resolvePerUser()
}

// ResolveClient returns the home runnyctl and other clients read and dial:
// SystemHomeDir when it EXISTS, else the per-user ~/.runny. Existence is the
// client signal — neither ownership (the operator reaches a system daemon's home
// through an ACL entry on the directory, never owning it) nor a connect probe:
// it selects WHICH home; the dial/read then reports a dead or unreadable one.
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
// fail loudly. The check is content-based, not spelling-based: strip the
// volume name (empty on unix, "C:" or a UNC prefix on windows) and reject if
// nothing but separators remains — that holds for every platform's spelling
// of "just the root" without needing to know it. A literal Clean(h) == "/"
// (or even Dir(Clean(h)) == Clean(h)) check doesn't reliably hold: windows'
// IsAbs/Clean treat a bare "//" and "///" inconsistently with each other, so
// pattern-matching the cleaned *string* is chasing edge cases the platform
// itself doesn't handle uniformly — the volume-stripped content check sidesteps
// that entirely.
func resolvePerUser() (Dir, error) {
	h, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home dir: %w", err)
	}
	rest := strings.TrimLeft(h[len(filepath.VolumeName(h)):], `/\`)
	if !filepath.IsAbs(h) || rest == "" {
		return "", fmt.Errorf("resolving home dir: implausible $HOME %q", h)
	}
	return Dir(filepath.Join(h, ".runny")), nil
}

func (d Dir) String() string { return string(d) }

// AtomicWrite writes data to path via a sibling temp file and a rename, so a
// crash or a failed write can never leave a torn file where a whole one was —
// a half-written config.yaml is a daemon that will not restart. The temp file
// is created in the destination's own directory (a rename across filesystems
// fails) with a random name (two writers must not collide on it), and is
// removed whenever the rename does not consume it.
//
// It does not chown: a caller writes into its OWN home, so the file lands owned
// by whoever is entitled to it. That is the point on a system home, where the
// daemon performs the write precisely so the config stays daemon-owned no
// matter which operator authored the edit. perm is explicit because the two
// callers differ -- a config the operator never opens stays 0600, a cycle
// artifact they do open is 0644 (see Ensure).
func AtomicWrite(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("creating a temp file beside %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the rename below consumes it
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", tmpPath, err)
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return fmt.Errorf("setting the mode on %s: %w", tmpPath, err)
	}
	// Sync before the rename, or the promise above is only about THIS process
	// dying: a rename can reach disk ahead of the bytes it points at, leaving a
	// zero-length file where a whole one was.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("flushing %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("placing %s: %w", path, err)
	}
	// The fsync above makes the BYTES durable; the rename is a change to the
	// parent DIRECTORY, and its entry can still be sitting in the page cache.
	// Without this, power loss right after a successful return leaves the data
	// safely on disk under the temp name and no config.yaml pointing at it.
	//
	// Best-effort: windows has no POSIX directory fsync, and a failure here is
	// never a failed write -- the rename has already succeeded and the caller's
	// data is in place. What is lost is durability across power loss, on a
	// platform that does not offer it.
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func (d Dir) ConfigPath() string { return filepath.Join(string(d), "config.yaml") }
func (d Dir) LockPath() string   { return filepath.Join(string(d), "runnyd.lock") }
func (d Dir) LogsDir() string    { return filepath.Join(string(d), "logs") }
func (d Dir) LogFile() string    { return filepath.Join(d.LogsDir(), "runnyd.log") }
func (d Dir) ImagesDir() string  { return filepath.Join(string(d), "images") }
func (d Dir) VMsDir() string     { return filepath.Join(string(d), "vms") }
func (d Dir) CyclesDir() string  { return filepath.Join(string(d), "cycles") }

// DockerConfigDir is where the daemon looks for registry credentials: a
// DOCKER_CONFIG directory (so it holds config.json, the standard name), not a
// file. The system daemon runs as a service account whose home is /var/empty,
// so the default ~/.docker/config.json is both absent and unwritable by the
// operator; pointing DOCKER_CONFIG here puts the credential somewhere the
// operator can author (they create the directory under their home-directory
// grant, and own what they create) and the daemon can read (its own entry
// inherits).
// Deliberately NOT a copy of the operator's own credential — a copy goes stale
// on rotation with nothing to notice; the file here is the one source, re-read
// on every pull attempt.
func (d Dir) DockerConfigDir() string { return filepath.Join(string(d), "docker") }

// RunnerCacheDir is the shared download store for actions-runner tarballs: the
// download happens once per version, not once per cycle, and fails fast before a
// boot. It is NOT mounted into a guest — each cycle CoW-clones its resolved
// tarball from here into its own SlotRunnerMountDir and mounts that. GC'd at
// cold start (images.PruneRunnerCache).
func (d Dir) RunnerCacheDir() string {
	return filepath.Join(string(d), "cache", "actions-runner")
}

// SlotRunnerMountDir is the per-slot virtiofs share: a subdir of the slot's
// ephemeral VMDir holding this cycle's single runner-tarball clone, mounted
// read-only into the guest. Nesting it under VMDir means the vms/ sweep and
// teardown's clone deletion reclaim it for free, and the cycle owns its tarball
// end to end — no other slot or store GC can touch it. Recreated each cycle
// (VMDir is); always deletable.
func (d Dir) SlotRunnerMountDir(slot string) string {
	return filepath.Join(d.VMDir(slot), "runner")
}

// ImageBundleDir is the cache location for one pulled image, keyed by
// registry reference and digest: images/<sanitized-ref>/sha256-<hex>/.
func (d Dir) ImageBundleDir(ref, digest string) string {
	return filepath.Join(d.ImagesDir(), sanitizeRef(ref), strings.ReplaceAll(digest, ":", "-"))
}

// ImageRefDir is the per-ref subdirectory of the image cache (the parent
// of all digest bundles for one ref). Used by the prune planner to compute
// the sanitized-ref key from a configured image ref.
func (d Dir) ImageRefDir(ref string) string {
	return filepath.Join(d.ImagesDir(), sanitizeRef(ref))
}

// VMDir is the ephemeral clone bundle for one slot. Always deletable.
func (d Dir) VMDir(slot string) string { return filepath.Join(d.VMsDir(), slot) }

// SlotCyclesDir holds the per-cycle artifact dirs for one slot.
func (d Dir) SlotCyclesDir(slot string) string { return filepath.Join(d.CyclesDir(), slot) }

// Ensure creates the directory skeleton. The HOME is owner-only and is the
// boundary that matters: the tree holds credentials-adjacent material end to
// end — App key path in config, the control socket, and post-mortem runner
// _diag logs that can contain unmasked job secrets — and a 0700 home is what
// keeps every one of them out of reach of anyone who is not the daemon or an
// operator.
//
// Two directories below it are deliberately readable rather than owner-only,
// which is not a relaxation of that: they sit INSIDE the 0700 home, so the same
// principals reach them and no others. It is how an operator opens the logs and
// cycle artifacts that `runnyctl why` and the docs point at, now that their ACL
// entry stops at the home directory. On windows mode bits are inert and the
// operator's inherited entry does this instead.
func (d Dir) Ensure() error {
	for _, e := range []struct {
		path string
		perm os.FileMode
	}{
		// The home at 0700 is the boundary. logs/ and cycles/ are readable
		// below it so an operator -- who reaches the home through its ACL entry
		// and no further -- can open what `runnyctl why` and the docs point
		// them at; they are inside a 0700 directory, so the home is still what
		// gates them. images/, vms/ and cache/ stay closed: nothing points an
		// operator at a VM disk, and prune reports them over the RPC.
		//
		// Mode bits are inert on windows, where the operator's inherited ACL
		// entry does this job instead (internal/sysdaemon's icaclsHomeArgs).
		{string(d), 0o700},
		{d.LogsDir(), 0o755},
		{d.CyclesDir(), 0o755},
		{d.ImagesDir(), 0o700},
		{d.VMsDir(), 0o700},
		{d.RunnerCacheDir(), 0o700},
	} {
		if err := os.MkdirAll(e.path, e.perm); err != nil {
			return fmt.Errorf("creating %s: %w", e.path, err)
		}
		// Re-assert on every start: MkdirAll leaves an existing directory's
		// mode alone, including one an older runny created at 0700.
		if err := os.Chmod(e.path, e.perm); err != nil {
			return fmt.Errorf("setting the mode on %s: %w", e.path, err)
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
