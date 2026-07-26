package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/bojanrajkovic/runny/internal/home"
	"github.com/bojanrajkovic/runny/internal/respawn"
	"github.com/bojanrajkovic/runny/internal/socket"
	"github.com/bojanrajkovic/runny/internal/testconfig"
)

// testConfigTimeout bounds the `<target> -test-config <config>` exec. The new
// binary is side-effect-free here (no home, no lock, no network), so 10 s is
// generous headroom, not a tight race — matching respawn's versionTimeout.
const testConfigTimeout = 10 * time.Second

// configTester is a seam for the `-test-config` exec so tests can fake the
// result without a real binary. The production implementation is execConfigTest.
type configTester func(ctx context.Context, targetPath, configPath string) bool

// execConfigTest is the production configTester: a deadline-bounded
// `<targetPath> -test-config <configPath>`, delegating the exec-and-parse to
// testconfig.RunTestConfig (shared with runnyctl's upgrade gate). Treats both
// "ok" and "warn" as acceptable — the operator already consented to warn-tier
// configs via `upgrade-daemon --force` (the gate that ran before we arrived
// here).
func execConfigTest(ctx context.Context, targetPath, configPath string) bool {
	cctx, cancel := context.WithTimeout(ctx, testConfigTimeout)
	defer cancel()
	v, err := testconfig.RunTestConfig(cctx, targetPath, configPath)
	if err != nil {
		return false
	}
	return v.Status == home.VerdictOK || v.Status == home.VerdictWarn
}

// deferralPlistPath returns the file naming the respawn target to consult for
// parse-deferral, or "" when there is none to consult -- either because this is
// not the system daemon, or because the platform has no staged-newer-binary
// state at all (systemRespawnTargetPath). Deferral is
// system-daemon-only: a per-user agent respawns from a bundle-relative
// BundleProgram, not this plist, so consulting it would test the wrong binary
// (mirrors respawn.TargetVersion's home guard, respawn.go). An empty path is the
// non-system signal parseDeferralCheck reads to leave the failure unchanged.
func deferralPlistPath(dir home.Dir) string {
	if dir.String() != home.SystemHomeDir {
		return ""
	}
	return systemRespawnTargetPath()
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
//
// ponytail: the respawn target's -test-config gates the LOCAL startup-blockers
// (localConfigChecks: key parse, guest-cap, namespace, image-ref) but NOT the
// network ones startup also hard-fails on (runner-perm, image-resolve,
// disk-headroom). That is deliberate and not a gap to close here: gating a
// forward-only migration on live GitHub/registry/disk health would let a
// transient blip refuse the very upgrade meant to fix things, and those failures
// are not silent — the slot FSM meets them loudly at MINT_JIT / ENSURE_IMAGE
// (backoff, why-visibility, cycle records), so a daemon with dead creds is no more
// "alive" refused than respawned. The network checks are point-in-time anyway
// (valid at gate-time, dead at runtime), so the FSM, not the gate, is the real net.
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
//     -test-config is authoritative, not this binary's parse or local checks. It
//     is consulted on EVERY exit attempt, never short-circuited on acceptedSHA:
//     a plain Reload joining the drain advances acceptedSHA to bytes validated
//     only against the OLD binary (UpdateAcceptedSHA), so a SHA match is not
//     proof the target vetted them — and an edit the old binary parses but the
//     new one rejects would otherwise slip through and crash-loop the respawn.
func exitConfigVerdict(ctx context.Context, log *slog.Logger, configPath, prefix, acceptedSHA string, deferred bool, plistPath string, test configTester) (bool, string) {
	cfg, sha, err := home.LoadConfigSHA(configPath)
	if deferred {
		if parseableByRespawnTarget(ctx, plistPath, configPath, test) {
			return true, ""
		}
		if err != nil {
			return false, fmt.Sprintf("config.yaml not accepted by the running binary or the respawn target: %v", err)
		}
		return false, "config.yaml not accepted by the respawn target"
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
