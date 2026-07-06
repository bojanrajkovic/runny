package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/bojanrajkovic/runny/internal/home"
	"github.com/bojanrajkovic/runny/internal/sysdaemon"
	runnyv1 "github.com/bojanrajkovic/runny/proto/runny/v1"
)

// configVerdict mirrors runnyd -test-config's JSON contract — the gate the new
// binary runs against the in-place config. The JSON is the contract; this struct
// tracks it (the Swift app keeps its own copy).
type configVerdict struct {
	Status   string           `json:"status"`
	Errors   []string         `json:"errors"`
	Warnings []verdictWarning `json:"warnings"`
}

type verdictWarning struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

const (
	verdictOK    = "ok"
	verdictWarn  = "warn"
	verdictError = "error"
)

// decideUpgrade maps a gate verdict status to whether to proceed with the
// drain-gated reload. Pure: the status + the --force flag fully determine it. OK
// proceeds; Warn proceeds only with --force; Error — and any unexpected status —
// refuses. --force overrides only the soft Warn tier, never a hard Error or an
// unrecognized verdict: the gate must never reload onto a config the new binary
// rejects.
func decideUpgrade(status string, force bool) (proceed bool, refusal string) {
	switch status {
	case verdictOK:
		return true, ""
	case verdictWarn:
		if force {
			return true, ""
		}
		return false, "config has warnings; re-run with --force to upgrade anyway"
	case verdictError:
		return false, "the new runnyd rejects the in-place config — upgrade refused (fix the config first)"
	default:
		return false, fmt.Sprintf("runnyd -test-config returned an unexpected status %q", status)
	}
}

// runConfigGate execs the on-disk runnyd's -test-config against configPath and
// returns the parsed verdict. The exit code mirrors the status (non-zero on
// error) but the JSON is the contract and is printed in every case, so the
// verdict is parsed from stdout regardless of the exit code; only a missing
// binary or unparseable output is a hard error.
func runConfigGate(runnydPath, configPath string) (configVerdict, error) {
	var out, errb bytes.Buffer
	cmd := exec.Command(runnydPath, "-test-config", configPath)
	cmd.Stdout, cmd.Stderr = &out, &errb
	runErr := cmd.Run()
	var v configVerdict
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &v); err != nil {
		if detail := strings.TrimSpace(errb.String()); runErr != nil && detail != "" {
			return configVerdict{}, fmt.Errorf("running %s -test-config: %v: %s", runnydPath, runErr, detail)
		} else if runErr != nil {
			return configVerdict{}, fmt.Errorf("running %s -test-config: %w", runnydPath, runErr)
		}
		return configVerdict{}, fmt.Errorf("%s -test-config produced no parseable verdict", runnydPath)
	}
	return v, nil
}

// upgradeDaemon gates a daemon update on the on-disk (newer) runnyd validating
// the in-place config, then — on OK (or Warn with --force) — issues the
// drain-gated reload that respawns the daemon onto the new binary. brew owns the
// binary delivery, so there is no re-stage here, and the daemon never
// self-upgrades: this is operator-driven.
func (c *ctl) upgradeDaemon(ctx context.Context, force bool, opts followOpts) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating runnyctl: %w", err)
	}
	runnyd := sysdaemon.ResolveRunnydPath(exe)
	if _, err := os.Stat(runnyd); err != nil {
		return fmt.Errorf("runnyd not found next to runnyctl at %s: %w", runnyd, err)
	}
	dir, err := home.ResolveClient()
	if err != nil {
		return err
	}
	v, err := runConfigGate(runnyd, dir.ConfigPath())
	if err != nil {
		return err
	}
	// Surface what the gate found regardless of the decision, so the operator sees
	// the warnings even when --force lets the upgrade proceed.
	for _, w := range v.Warnings {
		fmt.Fprintf(c.err, "warning: %s\n", w.Message)
	}
	for _, e := range v.Errors {
		fmt.Fprintf(c.err, "error: %s\n", e)
	}
	if proceed, refusal := decideUpgrade(v.Status, force); !proceed {
		return fmt.Errorf("%s", refusal)
	}
	// Narration goes to stderr — stdout is reloadWait's contract (the --json reload
	// document must not be preceded by prose).
	fmt.Fprintln(c.err, "config accepted by the new runnyd — draining and respawning onto it…")
	// Use UpgradeReload so the daemon can defer a config-parse failure to the
	// respawn target when the config contains a forward-only edit (a new key the
	// new binary accepts but the running binary's strict parser rejects). A
	// pre-feature daemon returns Unimplemented; catch it and tell the operator.
	upgradeReload := func(ctx context.Context, req *runnyv1.ReloadRequest) (*runnyv1.ReloadResponse, error) {
		resp, err := c.client.UpgradeReload(ctx, req)
		if status.Code(err) == codes.Unimplemented {
			return nil, fmt.Errorf("the running daemon predates upgrade-reload; " +
				"run `runnyctl reload --wait` instead (config-parse deferral unavailable)")
		}
		return resp, err
	}
	return c.reloadWait(ctx, "runnyctl upgrade-daemon", upgradeReload, opts)
}
