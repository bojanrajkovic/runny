package main

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/bojanrajkovic/runny/internal/home"
)

// configVerdict is the machine-readable result of `runnyd -test-config`: the
// cross-language contract the Swift app and runnyctl both parse to gate a daemon
// update. Status is ok|warn|error; errors and warnings are always arrays (never
// null). The schema is stable surface, versioned with the daemon.
type configVerdict struct {
	Status   string         `json:"status"`
	Errors   []string       `json:"errors"`
	Warnings []home.Warning `json:"warnings"`
}

const (
	verdictOK    = "ok"
	verdictWarn  = "warn"
	verdictError = "error"
)

// testConfigVerdict validates a config file with the LOCAL startup-blocking
// checks the respawn hard-fails on — strict parse + validate (LoadConfig), then
// the shared localConfigChecks (GitHub private-key parse, the macOS guest cap,
// the runner-name length cap against the given prefix, per-pool image-ref parse)
// — plus the soft-validation warnings, and returns the verdict. Sharing
// localConfigChecks with the daemon's exit gate is what keeps the gate from
// drifting out of sync with what startup enforces. It runs no network checks:
// upgrade-readiness is a question about config-schema compatibility, not live
// GitHub/registry/disk health, and a transient blip must never refuse a valid
// upgrade — the private-key check is a local file read + PEM/RSA parse, no
// round-trip. The prefix is injected (see gatePrefix) so the verdict is a pure
// function of its inputs.
func testConfigVerdict(configPath, prefix string, host home.HostResources) configVerdict {
	cfg, err := home.LoadConfig(configPath)
	if err != nil {
		return configVerdict{Status: verdictError, Errors: splitLines(err.Error()), Warnings: []home.Warning{}}
	}

	errs := []string{}
	for _, c := range localConfigChecks(cfg, prefix) {
		errs = append(errs, c.Name+": "+c.Detail)
	}

	warns := cfg.Warnings(host)
	if warns == nil {
		warns = []home.Warning{}
	}

	status := verdictOK
	switch {
	case len(errs) > 0:
		status = verdictError
	case len(warns) > 0:
		status = verdictWarn
	}
	return configVerdict{Status: status, Errors: errs, Warnings: warns}
}

// runTestConfig validates the config and prints the verdict JSON to stdout, then
// exits — status drives the exit code (0 for ok/warn, non-zero for error) but
// the JSON is the contract. Side-effect-free (no home, no lock, no network), so
// it works against an uninstalled binary and a misconfigured host. Called before
// any defers in run(), so the os.Exit on error skips none.
func runTestConfig(configPath string) error {
	v := testConfigVerdict(configPath, gatePrefix(), hostResources())
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshaling verdict: %w", err)
	}
	fmt.Println(string(b))
	if v.Status == verdictError {
		os.Exit(1)
	}
	return nil
}

// gatePrefix is the runner-name prefix the namespace check validates against:
// the persisted instance-id from the resolved home when it can be read (so the
// gate's verdict matches what the daemon validates at startup), else a
// conservative worst-case prefix that never under-estimates a hostname-derived
// one. The gate may over-refuse a borderline config but must never green-light
// one the respawn would reject — a silent post-upgrade crash-loop is the failure
// to avoid. Read-only: resolves the home by existence and reads instance-id, no
// writes, no network.
func gatePrefix() string {
	if dir, err := home.ResolveClient(); err == nil {
		if p, ok := dir.ReadInstancePrefix(); ok {
			return p
		}
	}
	return home.WorstCasePrefix()
}

// hostResources probes the local machine for the resource-overcommit warning:
// logical cores from the runtime, physical RAM from a platform probe (0 =
// unknown, which disables the RAM axis). No network.
func hostResources() home.HostResources {
	return home.HostResources{LogicalCores: runtime.NumCPU(), PhysicalRAMGB: physicalRAMGB()}
}

// splitLines splits a (possibly errors.Join'd, newline-separated) message into
// discrete entries for the JSON array.
func splitLines(s string) []string {
	return strings.Split(strings.TrimRight(s, "\n"), "\n")
}
