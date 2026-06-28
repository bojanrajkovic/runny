package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/bojanrajkovic/runny/internal/versioncore"
)

// upgradeCheckInterval is how often the daemon re-reads the on-disk binary's
// version. Coarse on purpose: a binary upgrade (a `brew upgrade`, an operator
// re-install) is an occasional event, and this is a courtesy log line for a
// headless operator reading logs — not a latency-sensitive signal. Each check
// execs the on-disk binary's side-effect-free -version, so a tight cadence would
// spend subprocesses for nothing.
const upgradeCheckInterval = 30 * time.Minute

// upgradeNotice logs once when the running daemon falls behind the binary launchd
// would respawn it as — the headless mirror of runnyctl's skew banner, for an
// operator who reads logs rather than running runnyctl. Log-only: the daemon
// never respawns or re-execs itself (a crash-only daemon's restarts come from
// launchd, not from itself); it only names the upgrade verb.
type upgradeNotice struct {
	log     *slog.Logger
	running string                       // this process's version core; "" for an unstamped dev build
	resolve func(context.Context) string // the respawn target's version core, or "" when it can't be read
	logged  string                       // the target core last logged as "behind"; "" = not currently warning
}

// observe logs the upgrade hint on the TRANSITION into "behind" for a given
// target core, and again only if the target core changes (a second upgrade
// landed) — never every tick. A same-core rebuild of the on-disk binary can't
// re-pop a dismissed warning, mirroring the app's banner. It stays QUIET when the
// running build is unstamped (dev, no core) or the target can't be resolved (""):
// a false "you're behind" is worse than silence.
func (u *upgradeNotice) observe(targetCore string) {
	if u.running == "" || targetCore == "" {
		return // unstamped self, or unresolved target: stay quiet, keep state
	}
	if versioncore.Compare(u.running, targetCore) < 0 {
		if targetCore != u.logged {
			u.log.Warn("a newer runnyd is available — run `runnyctl upgrade-daemon`",
				"running", u.running, "available", targetCore)
			u.logged = targetCore
		}
		return
	}
	u.logged = "" // caught up (or the on-disk binary went backward): re-arm
}

// run checks once at startup, then on every tick until ctx ends. Bounded by
// design: each check carries respawn.ExecRunner's deadline, and the loop stops
// with the daemon — no unbounded operation.
func (u *upgradeNotice) run(ctx context.Context) {
	check := func() { u.observe(u.resolve(ctx)) }
	check()
	t := time.NewTicker(upgradeCheckInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			check()
		}
	}
}
