package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/bojanrajkovic/runny/internal/home"
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

// regState is the tri-state result of a launchd registration probe. regUnknown is
// the fail-closed direction — a probe that wedged, timed out, or was denied is
// reported loudly, never mistaken for "absent".
type regState int

const (
	regUnknown regState = iota
	regAbsent
	regPresent
)

// competingProbeTimeout bounds the launchctl probe: a wedged launchd must not hang
// the doctor suite (the no-unbounded-operations invariant).
const competingProbeTimeout = 5 * time.Second

// launchctlPrint runs `launchctl print <target>` under a bound and returns its
// combined output. A package var so the doctor test fakes it without a live
// launchd; production never reassigns it.
var launchctlPrint = func(ctx context.Context, target string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, competingProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, "/bin/launchctl", "print", target).CombinedOutput()
	return string(out), err
}

// parseRegistration maps a `launchctl print` result to a registration state,
// mirroring sysdaemon.jobLoaded: a zero exit (the job dump) is present; a "could
// not find" (service absent in a reachable domain, OR no such domain because the
// operator isn't logged in — neither is a loaded competitor) is absent; anything
// else — a timeout, a permission denial, a wedged launchd — is Unknown, surfaced
// loudly rather than mistaken for absent.
func parseRegistration(out string, err error) regState {
	if err == nil {
		return regPresent
	}
	if strings.Contains(strings.ToLower(out), "could not find") {
		return regAbsent
	}
	return regUnknown
}

// competingRegistrationVerdict is the pure doctor verdict over the gathered facts,
// unit-tested without launchctl or a filesystem.
func competingRegistrationVerdict(systemHome, operatorResolved bool, guiTarget string, state regState) socket.DoctorCheck {
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
	switch state {
	case regPresent:
		return socket.DoctorCheck{
			Name: name, OK: false,
			Detail: fmt.Sprintf(
				"a per-user runnyd agent is registered (%s) and will contend for this home at the operator's next "+
					"GUI login — the second daemon to start loses the single-instance lock and exits; remove it with "+
					"`launchctl bootout %s` and delete its ~/Library/LaunchAgents plist",
				guiTarget, guiTarget,
			),
		}
	case regAbsent:
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
		return competingRegistrationVerdict(false, false, "", regUnknown)
	}
	uid, err := fileOwnerUID(configPath)
	if err != nil {
		return competingRegistrationVerdict(true, false, "", regUnknown)
	}
	guiTarget := fmt.Sprintf("gui/%d/%s", uid, sysdaemon.Label)
	out, perr := launchctlPrint(ctx, guiTarget)
	return competingRegistrationVerdict(true, true, guiTarget, parseRegistration(out, perr))
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
