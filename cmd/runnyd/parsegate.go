package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"time"

	"github.com/bojanrajkovic/runny/internal/home"
	"github.com/bojanrajkovic/runny/internal/respawn"
	"github.com/bojanrajkovic/runny/internal/socket"
	"github.com/bojanrajkovic/runny/internal/sysdaemon"
)

// testConfigTimeout bounds the `<target> -test-config <config>` exec. The new
// binary is side-effect-free here (no home, no lock, no network), so 10 s is
// generous headroom, not a tight race — matching respawn's versionTimeout.
const testConfigTimeout = 10 * time.Second

// configTester is a seam for the `-test-config` exec so tests can fake the
// result without a real binary. The production implementation is execConfigTest.
type configTester func(ctx context.Context, targetPath, configPath string) bool

// execConfigTest is the production configTester: a deadline-bounded
// `<targetPath> -test-config <configPath>`. Treats both "ok" and "warn" as
// acceptable — the operator already consented to warn-tier configs via
// `upgrade-daemon --force` (the gate that ran before we arrived here).
func execConfigTest(ctx context.Context, targetPath, configPath string) bool {
	cctx, cancel := context.WithTimeout(ctx, testConfigTimeout)
	defer cancel()
	// The exit code mirrors the verdict status but the JSON stdout is the
	// contract — parse it regardless of the exit code, same as runConfigGate.
	out, _ := exec.CommandContext(cctx, targetPath, "-test-config", configPath).Output()
	var v configVerdict
	if err := json.Unmarshal(bytes.TrimSpace(out), &v); err != nil {
		return false
	}
	return v.Status == verdictOK || v.Status == verdictWarn
}

// deferralPlistPath returns the system LaunchDaemon plist to consult for
// parse-deferral, or "" when the daemon is not the system daemon. Deferral is
// system-daemon-only: a per-user agent respawns from a bundle-relative
// BundleProgram, not this plist, so consulting it would test the wrong binary
// (mirrors respawn.TargetVersion's home guard, respawn.go). An empty path is the
// non-system signal parseDeferralCheck reads to leave the failure unchanged.
func deferralPlistPath(dir home.Dir) string {
	if dir.String() != home.SystemHomeDir {
		return ""
	}
	return sysdaemon.DefaultConfig().PlistPath()
}

// parseableByRespawnTarget returns true when the binary launchd would respawn
// the daemon as (read from plistPath, symlinks re-resolved NOW) accepts
// configPath via -test-config. ok=false when the respawn target can't be
// resolved — not the system daemon, no plist, missing symlink — in which case
// the caller keeps the strict own-parse hold.
func parseableByRespawnTarget(ctx context.Context, plistPath, configPath string, test configTester) bool {
	path, ok := respawn.TargetPath(plistPath)
	if !ok {
		return false
	}
	return test(ctx, path, configPath)
}

// parseDeferralCheck applies the RPC-gated respawn-target fallback to a lone
// config-parse failure. When deferred and the respawn target accepts the config
// (a forward-only edit the old parser rejects but the new one understands), the
// failed list is cleared. When deferred but the target also refuses — the
// stale-symlink guard: the target IS the old binary, which rejects for the same
// reason — a synthetic respawn-target-config failure is returned so the operator
// has an actionable error. When not deferred or the failure is not a lone
// config-parse, failed is returned unchanged.
//
// An empty plistPath means the daemon is not the system daemon (deferralPlistPath
// returned "" off the system home): deferral is system-daemon-only, so the
// original config-parse failure stands unchanged — not the respawn-target-config
// synthetic, which would point a per-user agent at an irrelevant symlink.
func parseDeferralCheck(ctx context.Context, configPath, plistPath string, failed []socket.DoctorCheck, deferred bool, test configTester) []socket.DoctorCheck {
	if !deferred || plistPath == "" || len(failed) != 1 || failed[0].Name != "config-parse" {
		return failed
	}
	if parseableByRespawnTarget(ctx, plistPath, configPath, test) {
		return nil // respawn target accepts: proceed to drain
	}
	return []socket.DoctorCheck{{
		Name: "respawn-target-config", OK: false,
		Detail: "config not accepted by the running binary or the respawn target; " +
			"verify the upgraded binary is staged and the opt-symlink resolves to it",
	}}
}

// exitConfigVerdict is the exit gate: before the drained daemon hands off to
// launchd, prove the on-disk config the respawn will load is one the RESPAWN
// TARGET accepts. Returns (ok, holdDetail); ok=false HOLDS the drained daemon
// rather than crash-loop a socketless respawn.
//
// The authority is whichever binary will actually respawn:
//   - Normal drain (not deferred): the respawn is THIS binary, so run its local
//     startup-blocking checks directly (localConfigChecks) — no network at the
//     exit seam, where a refusal would have no good answer.
//   - UpgradeReload drain (deferred): the respawn is a NEWER binary, so its
//     -test-config is authoritative, not this binary's parse or local checks.
//     Bytes already vetted against it at preflight (parseable here AND unchanged)
//     need no re-exec; a parse failure or a mid-drain edit (sha != acceptedSHA)
//     is (re)validated against it — without this, an edit the old binary parses
//     but the new one rejects would slip through and crash-loop the respawn.
func exitConfigVerdict(ctx context.Context, log *slog.Logger, configPath, prefix, acceptedSHA string, deferred bool, plistPath string, test configTester) (bool, string) {
	cfg, sha, err := home.LoadConfigSHA(configPath)
	if deferred {
		if err == nil && acceptedSHA != "" && sha == acceptedSHA {
			return true, "" // these exact bytes were vetted against the target at preflight
		}
		if parseableByRespawnTarget(ctx, plistPath, configPath, test) {
			return true, ""
		}
		if err != nil {
			return false, fmt.Sprintf("config.yaml not accepted by the running binary or the respawn target: %v", err)
		}
		return false, "config.yaml changed during the drain and the respawn target rejects it"
	}
	if err != nil {
		return false, fmt.Sprintf("config.yaml no longer parses; the respawn would refuse it: %v", err)
	}
	if failed := localConfigChecks(cfg, prefix); len(failed) > 0 {
		return false, fmt.Sprintf("the respawn would refuse %s: %s", failed[0].Name, failed[0].Detail)
	}
	if acceptedSHA != "" && sha != acceptedSHA {
		log.Warn("config changed during the drain; the respawn will validate and load the newer file",
			"accepted_sha256", acceptedSHA, "current_sha256", sha)
	}
	return true, ""
}
