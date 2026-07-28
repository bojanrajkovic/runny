package main

import (
	"context"
	"fmt"
	"os"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/bojanrajkovic/runny/internal/home"
	"github.com/bojanrajkovic/runny/internal/testconfig"
	runnyv1 "github.com/bojanrajkovic/runny/proto/runny/v1"
)

// decideUpgrade maps a gate verdict status to whether to proceed with the
// drain-gated reload. Pure: the status + the --force flag fully determine it. OK
// proceeds; Warn proceeds only with --force; Error — and any unexpected status —
// refuses. --force overrides only the soft Warn tier, never a hard Error or an
// unrecognized verdict: the gate must never reload onto a config the new binary
// rejects.
func decideUpgrade(status string, force bool) (proceed bool, refusal string) {
	switch status {
	case home.VerdictOK:
		return true, ""
	case home.VerdictWarn:
		if force {
			return true, ""
		}
		return false, "config has warnings; re-run with --force to upgrade anyway"
	case home.VerdictError:
		return false, "the new runnyd rejects the in-place config — upgrade refused (fix the config first)"
	default:
		return false, fmt.Sprintf("runnyd -test-config returned an unexpected status %q", status)
	}
}

// stageConfigForValidation writes the daemon's current config to a temp file
// this process can read, and returns its path (the caller removes it). The
// gate it feeds must run the NEW binary against the config, so it cannot be
// delegated to the running daemon — the whole point is to learn whether a
// runnyd this daemon is not yet running accepts the file.
func (c *ctl) stageConfigForValidation(ctx context.Context, configPath string) (string, error) {
	content, err := c.configBytes(ctx, configPath)
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp("", "runny-config-*.yaml")
	if err != nil {
		return "", fmt.Errorf("creating a temp file: %w", err)
	}
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", fmt.Errorf("staging %s for validation: %w", configPath, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("staging %s for validation: %w", configPath, err)
	}
	return tmp.Name(), nil
}

// upgradeDaemon gates a daemon update on the on-disk (newer) runnyd validating
// the in-place config, then — on OK (or Warn with --force) — issues the
// drain-gated reload that respawns the daemon onto the new binary. brew owns the
// binary delivery, so there is no re-stage here, and the daemon never
// self-upgrades: this is operator-driven.
func (c *ctl) upgradeDaemon(ctx context.Context, force bool, opts followOpts) error {
	runnyd, err := resolveRunnyd()
	if err != nil {
		return err
	}
	dir, err := home.ResolveClient()
	if err != nil {
		return err
	}
	// Validate a COPY, not the live file. `runnyd -test-config` runs as the
	// operator, and on a system home the operator has no access to config.yaml
	// — its ACL entry stops at the home directory — so the bytes come over the
	// control channel and land somewhere this process can read. Validation is
	// not path-sensitive (see editConfig), so the copy's verdict is the file's.
	configCopy, err := c.stageConfigForValidation(ctx, dir.ConfigPath())
	if err != nil {
		return err
	}
	defer os.Remove(configCopy)
	v, err := testconfig.RunTestConfig(ctx, runnyd, configCopy)
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
