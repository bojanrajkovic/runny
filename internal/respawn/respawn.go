// Package respawn resolves the binary launchd would respawn the running daemon
// as, and reads its version. It exists to get one subtle thing right:
// os.Executable() on the running process returns the path it was STARTED from,
// which after a `brew upgrade` is the OLD cellar binary even though the stable
// opt-symlink now points at the NEW one. So the respawn target is read from the
// daemon's own LaunchDaemon plist (ProgramArguments[0]) and its symlinks are
// re-resolved NOW — never from os.Executable, which would always report the
// running version and so could never detect that a newer binary is on disk.
//
// Every unresolved case fails QUIET (ok=false): not the system daemon, no plist,
// an unreadable or malformed plist, a vanished target, a failed exec. A caller
// then stays silent rather than warning wrongly — a false "you're behind" is
// worse than no hint at all.
package respawn

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"howett.net/plist"

	"github.com/bojanrajkovic/runny/internal/home"
	"github.com/bojanrajkovic/runny/internal/sysdaemon"
)

// versionTimeout bounds the `<target> -version` exec: a wedged or hung child
// binary must not stall the daemon. The child is side-effect-free (`-version`
// reads no home, takes no lock, touches no network), so this is generous
// headroom, not a tight race. A plain context.WithTimeout, matching the other
// local-exec seams (sysdaemon, launchd) — bounded.Context is reserved for
// network/guest seams, which this is not.
const versionTimeout = 10 * time.Second

// Runner execs the resolved respawn target with -version and returns its trimmed
// output. A seam so tests fake the exec without a real binary; production passes
// ExecRunner.
type Runner func(ctx context.Context, path string) (string, error)

// ExecRunner is the production Runner: a deadline-bounded `<path> -version`. The
// bound is load-bearing — without it a hung child binary would hang the daemon.
func ExecRunner(ctx context.Context, path string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, path, "-version").Output()
	return strings.TrimSpace(string(out)), err
}

// TargetVersion resolves the binary launchd would respawn the daemon at home as
// and reads its raw version string. ok=false (stay quiet) when the daemon is not
// the system daemon — a per-user agent respawns from a bundle-relative
// BundleProgram, out of scope here — or the respawn target can't be resolved or
// read.
func TargetVersion(ctx context.Context, h home.Dir) (version string, ok bool) {
	// Channel: only the system daemon respawns from a LaunchDaemon plist. A
	// per-user agent at ~/.runny has none — stay quiet without touching disk.
	if h.String() != home.SystemHomeDir {
		return "", false
	}
	return targetVersion(ctx, sysdaemon.DefaultConfig().PlistPath(), ExecRunner)
}

func targetVersion(ctx context.Context, plistPath string, run Runner) (string, bool) {
	path, ok := resolveProgramPath(plistPath)
	if !ok {
		return "", false
	}
	v, err := run(ctx, path)
	if err != nil || v == "" {
		return "", false
	}
	return v, true
}

// resolveProgramPath reads ProgramArguments[0] from a LaunchDaemon plist and
// re-resolves its symlinks NOW. The EvalSymlinks is the whole point: the plist
// names a stable opt-symlink (so `brew upgrade` need not rewrite the plist), and
// resolving it at read time yields the CURRENTLY-installed binary, which is what
// launchd would exec on the next cold start. It deliberately does NOT consult
// os.Executable — that would pin the running (pre-upgrade) path and defeat the
// detection. ok=false on any failure so the caller stays quiet.
func resolveProgramPath(plistPath string) (string, bool) {
	data, err := os.ReadFile(plistPath)
	if err != nil {
		return "", false
	}
	var p struct {
		ProgramArguments []string `plist:"ProgramArguments"`
	}
	if _, err := plist.Unmarshal(data, &p); err != nil {
		return "", false
	}
	if len(p.ProgramArguments) == 0 || p.ProgramArguments[0] == "" {
		return "", false
	}
	resolved, err := filepath.EvalSymlinks(p.ProgramArguments[0])
	if err != nil {
		return "", false
	}
	return resolved, true
}
