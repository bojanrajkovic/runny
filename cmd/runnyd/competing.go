package main

import (
	"context"
	"fmt"
	"os"
	"syscall"

	"github.com/bojanrajkovic/runny/internal/home"
	"github.com/bojanrajkovic/runny/internal/launchd"
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
			Detail: "couldn't determine the operator account (the owner of config.yaml) to probe for a competing per-user agent",
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

// checkCompetingRegistration gathers the facts and returns the verdict. Darwin-only
// (launchctl); the caller gates on GOOS. The operator is the owner of config.yaml —
// for a system-daemon home that file is operator-owned 0600, so its owner is the
// account whose gui/ domain a leftover per-user agent would live in.
func checkCompetingRegistration(ctx context.Context, dir home.Dir, configPath string) socket.DoctorCheck {
	if dir.String() != home.SystemHomeDir {
		return competingRegistrationVerdict(false, false, "", launchd.Indeterminate)
	}
	uid, err := fileOwnerUID(configPath)
	if err != nil {
		return competingRegistrationVerdict(true, false, "", launchd.Indeterminate)
	}
	guiTarget := fmt.Sprintf("gui/%d/%s", uid, sysdaemon.Label)
	return competingRegistrationVerdict(true, true, guiTarget, launchd.Probe(ctx, launchdRunner, guiTarget))
}

// fileOwnerUID returns the uid that owns path.
func fileOwnerUID(path string) (uint32, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("stat %s: no unix owner info", path)
	}
	return st.Uid, nil
}
