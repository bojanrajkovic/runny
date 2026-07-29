package main

import (
	"context"
	"fmt"

	"github.com/bojanrajkovic/runny/internal/home"
	"github.com/bojanrajkovic/runny/internal/launchd"
	"github.com/bojanrajkovic/runny/internal/opacl"
	"github.com/bojanrajkovic/runny/internal/socket"
	"github.com/bojanrajkovic/runny/internal/sysdaemon"
)

// The competing-registration doctor check surfaces, in one command, the one
// cross-shape conflict the daemon-ownership model deliberately no longer tries to
// auto-resolve: a per-user runnyd LaunchAgent registered alongside the installed
// system LaunchDaemon. When the operator logs into a GUI, that agent loads and
// contends for the same home — the second runnyd to start loses the single-instance
// flock and exits, loud in a log nobody reads on a headless host. This makes that
// latent conflict queryable via `runnyd -doctor` and `runnyctl doctor`.
//
// It only applies to a system-home deployment: a per-user deployment can't hide a
// co-present system daemon, because the system home would have won client home
// resolution (so we'd be the system daemon, not a per-user one).

// launchdRunner is the launchd probe seam, overridden in tests; production uses the
// bounded `launchctl print`.
var launchdRunner launchd.Runner = launchd.ExecRunner

// competingRegistrationVerdict is the pure doctor verdict over the gathered facts,
// unit-tested without launchctl or a filesystem.
func competingRegistrationVerdict(systemHome, operatorResolved bool, guiTarget string, reg launchd.Result) socket.DoctorCheck {
	const name = "competing-registration"
	if !systemHome {
		return socket.DoctorCheck{Name: name, OK: true, Detail: "per-user home — no competing registration to detect"}
	}
	if !operatorResolved {
		return socket.DoctorCheck{
			Name: name, OK: false,
			Detail: "couldn't determine the operator set (the home directory's ACL) to probe for a competing per-user agent",
		}
	}
	switch reg {
	case launchd.Registered:
		return socket.DoctorCheck{
			Name: name, OK: false,
			Detail: fmt.Sprintf(
				"a per-user runnyd agent is registered (%s) and will contend for this home at the operator's next "+
					"GUI login — the second daemon to start loses the single-instance lock and exits; remove it with "+
					"`launchctl bootout %s` and delete its ~/Library/LaunchAgents plist",
				guiTarget, guiTarget,
			),
		}
	case launchd.NotRegistered:
		return socket.DoctorCheck{Name: name, OK: true, Detail: "no competing per-user runnyd agent is registered"}
	default:
		return socket.DoctorCheck{
			Name: name, OK: false,
			Detail: fmt.Sprintf(
				"couldn't determine whether a competing per-user runnyd agent is registered — the launchctl probe "+
					"of %s was inconclusive (a denied or wedged probe, not a clean absence)", guiTarget,
			),
		}
	}
}

// operatorIDs reads the operator set. It is the same registry the per-RPC
// revocation gate and `runnyctl operator list` read -- the home directory's ACL
// -- and it is a seam only so tests can drive the check without one. Overridden
// in tests; production is opacl.ListIDs.
var operatorIDs = opacl.ListIDs

// checkCompetingRegistration gathers the facts and returns the verdict. Darwin-only
// (launchctl); the caller gates on GOOS.
//
// It probes EVERY operator's gui/ domain, because a stale agent lives in exactly
// one human's domain and nothing says which. The operator set comes from the
// home's ACL: that is what "operator" means everywhere else in this daemon, and
// it is the only source that stays correct as grants and revokes happen.
//
// It used to key on the OWNER of config.yaml instead, which was wrong in three
// ways at once -- it saw one account where the question is about a set, so a
// stale agent in a second operator's domain read as a confident green; the
// owner need not be an operator at all, since ownership does not change when a
// grant does; and once the daemon began writing config.yaml itself the owner
// became the service account, which has no gui/ domain by construction and so
// could only ever return "inconclusive". A check that cannot fail is worse than
// no check.
func checkCompetingRegistration(ctx context.Context, dir home.Dir) socket.DoctorCheck {
	if dir.String() != home.SystemHomeDir {
		return competingRegistrationVerdict(false, false, "", launchd.Indeterminate)
	}
	ids, err := operatorIDs(dir.String())
	if err != nil || len(ids) == 0 {
		return competingRegistrationVerdict(true, false, "", launchd.Indeterminate)
	}
	// A probe that could not answer is not a clean absence -- the agent could be
	// in exactly the domain that refused -- so one inconclusive result outranks
	// any number of clean ones. A registered agent outranks everything and stops
	// the walk: it is already the answer, and the remediation names it.
	inconclusive := ""
	for _, id := range ids {
		target := fmt.Sprintf("gui/%s/%s", id, sysdaemon.Label)
		switch launchd.Probe(ctx, launchdRunner, target) {
		case launchd.Registered:
			return competingRegistrationVerdict(true, true, target, launchd.Registered)
		case launchd.Indeterminate:
			if inconclusive == "" {
				inconclusive = target
			}
		}
	}
	if inconclusive != "" {
		return competingRegistrationVerdict(true, true, inconclusive, launchd.Indeterminate)
	}
	return competingRegistrationVerdict(true, true, "", launchd.NotRegistered)
}
