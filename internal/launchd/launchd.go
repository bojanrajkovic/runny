// Package launchd probes whether a launchd job is registered, via a bounded
// `launchctl print`. It is the Go sibling of the app's Swift LaunchdProbe and the
// shared home for the registration probe used by both `runnyd -doctor` (the
// competing-registration check) and `runnyctl install-daemon` (the per-user-agent
// guard) — the no-unbounded-operations invariant applied to a local launchctl exec.
package launchd

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// Result is the tri-state outcome of a registration probe, mirroring the Swift
// LaunchdProbeResult. Indeterminate is the zero value and the fail-closed
// direction — a probe that wedged, was denied, or timed out is reported as
// Indeterminate, never mistaken for NotRegistered.
type Result int

const (
	Indeterminate Result = iota
	NotRegistered
	Registered
)

func (r Result) String() string {
	switch r {
	case Registered:
		return "registered"
	case NotRegistered:
		return "not-registered"
	default:
		return "indeterminate"
	}
}

// probeTimeout bounds one `launchctl print`: a wedged launchd must not hang the
// caller (a doctor suite, an install command).
const probeTimeout = 5 * time.Second

// Runner runs `launchctl print <target>` and returns its combined output. A seam
// so callers' tests fake it without a live launchd; production passes ExecRunner.
type Runner func(ctx context.Context, target string) (string, error)

// ExecRunner is the production Runner: a bounded `launchctl print <target>`.
func ExecRunner(ctx context.Context, target string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, "/bin/launchctl", "print", target).CombinedOutput()
	return string(out), err
}

// Classify maps a `launchctl print` result to a Result, mirroring
// sysdaemon.jobLoaded: a zero exit (the job dump) is Registered; a "could not
// find" (the service is absent in a reachable domain, OR there is no such domain
// because the user isn't logged in — neither is a loaded job) is NotRegistered;
// anything else — a timeout, a permission denial, a wedged launchd — is
// Indeterminate, surfaced loudly rather than mistaken for an absence.
func Classify(out string, err error) Result {
	if err == nil {
		return Registered
	}
	if strings.Contains(strings.ToLower(out), "could not find") {
		return NotRegistered
	}
	return Indeterminate
}

// Probe runs the probe through run and classifies the result. target is a launchd
// selector like `system/<label>` or `gui/<uid>/<label>`.
func Probe(ctx context.Context, run Runner, target string) Result {
	out, err := run(ctx, target)
	return Classify(out, err)
}
