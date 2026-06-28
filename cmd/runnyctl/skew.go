package main

import (
	"context"
	"fmt"

	"github.com/bojanrajkovic/runny/internal/versioncore"
	runnyv1 "github.com/bojanrajkovic/runny/proto/runny/v1"
)

// expectedProtocolVersion is the wire-protocol version runnyctl's generated
// stubs were built against — the exact protocol it expects, kept in lockstep
// with the daemon's socket.WireProtocolVersion (bump both together). The
// protocol axis warns only when the daemon is BEHIND this; a newer daemon
// degrades nothing. This is not a backstop or a cap — the healthy-magnitude
// sizing rule does not apply.
const expectedProtocolVersion uint32 = 2

// versionSkew classifies the skew between this runnyctl and the runnyd it dials,
// returning a one-line warning and whether to print it. Warn, never refuse — the
// CLI mirror of the app's skew verdict: a runnyctl that came from the .app bundle
// can lag a brew-managed runnyd on a shared host. Two independent axes, neither
// implied by the other:
//   - version mismatch: the normalized x.y.z cores differ (the shared-host case).
//   - protocol behind: the cores match but the daemon's protocol is below what
//     this CLI's stubs expect — the upgrade window the matched cores hide; '<',
//     not '!=', so a newer daemon serving an older CLI never warns.
//
// Quiet when the daemon's version is not known yet (a daemon predating the
// field), or when this is an unstamped dev build (version "dev" has no core), so
// a dev build never wears a false warning.
func versionSkew(cliVersion, daemonVersion string, cliProto, daemonProto uint32) (string, bool) {
	daemonCore := versioncore.Core(daemonVersion)
	if daemonCore == "" {
		return "", false
	}
	cliCore := versioncore.Core(cliVersion)
	if cliCore == "" {
		return "", false
	}
	if cliCore != daemonCore {
		// Name the direction. A daemon BEHIND this (newer) runnyctl is the
		// actionable headless case — a brew upgrade landed a newer runnyctl and the
		// running daemon lags, so name the verb. A daemon AHEAD means runnyctl is the
		// lagging install (never suggest upgrading the daemon down).
		if versioncore.Compare(daemonCore, cliCore) < 0 {
			return fmt.Sprintf(
				"warning: a newer runnyd is available (runnyctl %s, runnyd %s) — run `runnyctl upgrade-daemon`",
				cliCore, daemonCore,
			), true
		}
		return fmt.Sprintf(
			"warning: runnyctl is %s but runnyd is %s — different releases; upgrade the lagging install",
			cliCore, daemonCore,
		), true
	}
	if daemonProto < cliProto {
		return "warning: this runnyd predates a capability runnyctl expects — some features may " +
			"not work; upgrade or restart runnyd", true
	}
	return "", false
}

// warnSkew prints a one-line version-skew warning to stderr before a command's
// own output, on every daemon-dialing path. It is best-effort: one bounded
// GetStatus that a down or wedged daemon fails fast (the command itself then
// surfaces the real error), so a skew check never blocks or hangs a command. The
// deliberate extra status read (status/watch fetch their own afterward) buys a
// uniform warning no command path can silently omit.
func (c *ctl) warnSkew(ctx context.Context) {
	pctx, cancel := context.WithTimeout(ctx, probeCallTimeout)
	defer cancel()
	resp, err := c.client.GetStatus(pctx, &runnyv1.GetStatusRequest{})
	if err != nil {
		return
	}
	if w, skewed := versionSkew(version, resp.GetVersion(), expectedProtocolVersion, resp.GetProtocolVersion()); skewed {
		fmt.Fprintln(c.err, w)
	}
}
