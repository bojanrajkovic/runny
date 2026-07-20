package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"runtime"

	"github.com/bojanrajkovic/runny/internal/home"
	"github.com/bojanrajkovic/runny/internal/launchd"
	"github.com/bojanrajkovic/runny/internal/sysdaemon"
)

// installDaemon installs runnyd as a non-root system LaunchDaemon. It is a
// privileged local command (run via `sudo runnyctl install-daemon`), never a
// daemon RPC — the daemon does not exist yet. The plist points at the runnyd
// sibling of this runnyctl, and the inheriting ACL grants the operator account
// (the --operator flag, else the human who ran sudo via SUDO_USER). Both
// arguments arrive already parsed by kong (see InstallDaemonCmd).
func installDaemon(operatorFlag, configFlag string) error {
	if err := requireInstallPrivilege("install-daemon"); err != nil {
		return err
	}
	operator, err := resolveOperator(operatorFlag, operatorFallback())
	if err != nil {
		return err
	}
	if err := refuseSystemOperator(operator); err != nil {
		return err
	}
	if err := preflightPerUserAgent(operator); err != nil {
		return err
	}
	runnyd, err := resolveRunnyd()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w\n  for a from-checkout build, use tools/deploy/install-system.sh with RUNNYD=/path/to/runnyd", err)
		}
		return err
	}
	cfg := sysdaemon.Config{Operator: operator, RunnydPath: runnyd}
	installer := sysdaemon.New(cfg)
	if configFlag != "" {
		plan, err := planStageFromFile(configFlag, home.SystemHomeDir, operator)
		if err != nil {
			return err
		}
		installer = installer.WithStage(plan)
	}
	if err := installer.Install(context.Background()); err != nil {
		return err
	}
	dir := home.Dir(home.SystemHomeDir)
	if configFlag != "" {
		fmt.Printf("\nrunnyd is installed, started, and running on the staged config (validated by\n"+
			"runnyd -test-config before start). Change it later with `runnyctl edit-config`\n"+
			"— never hand-edit %s and restart.\n", dir.ConfigPath())
		return nil
	}
	if runtime.GOOS == "windows" {
		fmt.Printf("\nrunnyd is installed and started as the %s service. Until a valid config is in\n"+
			"place it crash-loops (expected — not a hang; check `sc.exe query %s` and watch\n"+
			"%s\\service.err.log). Next:\n"+
			"  1. write %s (your account has write access via the ACL)\n"+
			"  2. place the GitHub App key where its private_key_path points\n"+
			"  3. runnyctl doctor   — the daemon comes up on its next restart\n",
			sysdaemon.WindowsServiceName, sysdaemon.WindowsServiceName, dir.LogsDir(), dir.ConfigPath())
		return nil
	}
	fmt.Printf("\nrunnyd is installed and started as %s. Until a valid config is in place it\n"+
		"crash-loops (expected — not a hang; watch %s/launchd.err.log). Next:\n"+
		"  1. write %s (your account has write access via the ACL)\n"+
		"  2. place the GitHub App key where its private_key_path points\n"+
		"  3. runnyctl doctor   — the daemon comes up on its next restart\n",
		sysdaemon.ServiceUser, dir.LogsDir(), dir.ConfigPath())
	return nil
}

// planStageFromFile reads and parses configPath, resolves operator's home for
// ~ expansion, computes the StagePlan, and pre-flights every planned key's
// source file (stat only — nothing is copied yet) so a missing key aborts
// before install touches the filesystem, rather than mid-stage.
func planStageFromFile(configPath, homeDir, operator string) (sysdaemon.StagePlan, error) {
	// Read once and parse those exact bytes — a separate re-read for the parse
	// could see a different version of the file (a concurrent edit, or a slow
	// mount) than the one PlanStage rewrites, the same hazard LoadConfigSHA
	// reads once to avoid.
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return sysdaemon.StagePlan{}, fmt.Errorf("reading %s: %w", configPath, err)
	}
	cfg, err := home.ParseConfig(raw, configPath)
	if err != nil {
		return sysdaemon.StagePlan{}, err
	}
	u, err := user.Lookup(operator)
	if err != nil {
		return sysdaemon.StagePlan{}, fmt.Errorf("resolving %s's home for ~ expansion: %w", operator, err)
	}
	plan, err := sysdaemon.PlanStage(raw, cfg, homeDir, u.HomeDir)
	if err != nil {
		return sysdaemon.StagePlan{}, err
	}
	for _, k := range plan.Keys {
		if _, err := os.Stat(k.Src); err != nil {
			return sysdaemon.StagePlan{}, fmt.Errorf("private key %s (from %s): %w", k.Src, configPath, err)
		}
	}
	return plan, nil
}

// uninstallDaemon removes the system LaunchDaemon. It leaves the service account
// and the home (config, key, artifacts) intact — see sysdaemon.Installer.Uninstall.
// It takes no arguments; kong rejects any before this runs (see UninstallDaemonCmd).
func uninstallDaemon() error {
	if err := requireInstallPrivilege("uninstall-daemon"); err != nil {
		return err
	}
	return sysdaemon.New(sysdaemon.Config{}).Uninstall(context.Background())
}

// resolveRunnyd locates the runnyd sibling of the running runnyctl (via
// sysdaemon.ResolveRunnydPath) and confirms it exists, returning an error
// naming the expected path when it's not there. Shared by every command that
// execs the on-disk runnyd (install-daemon, edit-config, upgrade-daemon).
func resolveRunnyd() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locating runnyctl: %w", err)
	}
	runnyd := sysdaemon.ResolveRunnydPath(exe)
	if _, err := os.Stat(runnyd); err != nil {
		return "", fmt.Errorf("runnyd not found next to runnyctl at %s: %w", runnyd, err)
	}
	return runnyd, nil
}

// resolveOperator selects the operator account from the explicit --operator flag
// or $SUDO_USER. The explicit flag wins: a root invocation without sudo (e.g. CI,
// or `su root`) leaves SUDO_USER unset, so the operator can only travel by flag
// there; the headless `sudo runnyctl install-daemon` path passes no flag and
// resolves off SUDO_USER.
// `root` is refused from either source — an ACL grant to root is pointless and
// signals a non-login invocation. The local-user check lives in Install(), not
// here, so source selection stays pure and unit-testable.
func resolveOperator(flagOperator, sudoUser string) (string, error) {
	op := flagOperator
	if op == "" {
		op = sudoUser
	}
	if op == "" || op == "root" {
		return "", fmt.Errorf("cannot determine the operator account: pass `--operator <user>`, or run " +
			"via `sudo runnyctl install-daemon` from your normal login so SUDO_USER is set (the home's " +
			"ACL grants that account access)")
	}
	return op, nil
}

// perUserAgentGuard is the pure guard decision over the operator and the probe
// result. A registered per-user agent is a hard refusal. An inconclusive probe does
// NOT block — a transient launchctl failure must not stop a privileged install, so
// it warns instead, and the competing-registration doctor check plus the
// single-instance flock are the remaining safety nets.
func perUserAgentGuard(operator, guiTarget string, reg launchd.Result) (refuse error, warning string) {
	switch reg {
	case launchd.Registered:
		return fmt.Errorf(
			"a per-user runnyd LaunchAgent is registered for %s (%s) — installing the system daemon over it would "+
				"strand the per-user daemon orphaned behind it. Withdraw it first (toggle the daemon off in the Runny "+
				"app, or `launchctl bootout %s`), then re-run `sudo runnyctl install-daemon`",
			operator, guiTarget, guiTarget,
		), ""
	case launchd.Indeterminate:
		return nil, fmt.Sprintf(
			"couldn't verify whether a per-user runnyd agent is registered for %s (launchctl probe inconclusive); "+
				"if `runnyctl doctor` later flags a competing registration, withdraw the per-user agent", operator,
		)
	}
	return nil, ""
}
