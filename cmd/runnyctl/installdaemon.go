package main

import (
	"context"
	"flag"
	"fmt"
	"io"
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
// (the --operator flag, else the human who ran sudo via SUDO_USER).
func installDaemon(args []string) error {
	fs := flag.NewFlagSet("install-daemon", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	operatorFlag := fs.String("operator", "",
		"operator account the home's inheriting ACL grants (defaults to $SUDO_USER; required "+
			"when not run via sudo — the app's brokered install runs as root with no SUDO_USER set)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("install-daemon takes no positional arguments (got %q)", fs.Arg(0))
	}
	if err := requireDarwinRoot("install-daemon"); err != nil {
		return err
	}
	operator, err := resolveOperator(*operatorFlag, os.Getenv("SUDO_USER"))
	if err != nil {
		return err
	}
	if err := checkNoPerUserAgent(operator); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating runnyctl: %w", err)
	}
	runnyd := sysdaemon.ResolveRunnydPath(exe)
	if _, err := os.Stat(runnyd); err != nil {
		return fmt.Errorf("runnyd not found next to runnyctl at %s: %w\n"+
			"  for a from-checkout build, use tools/deploy/install-system.sh with RUNNYD=/path/to/runnyd", runnyd, err)
	}
	cfg := sysdaemon.DefaultConfig()
	cfg.Operator = operator
	cfg.RunnydPath = runnyd
	if err := sysdaemon.New(cfg).Install(context.Background()); err != nil {
		return err
	}
	dir := home.Dir(cfg.HomeDir)
	fmt.Printf("\nrunnyd is installed and started as %s. Until a valid config is in place it\n"+
		"crash-loops (expected — not a hang; watch %s/launchd.err.log). Next:\n"+
		"  1. write %s (your account has write access via the ACL)\n"+
		"  2. place the GitHub App key where its private_key_path points\n"+
		"  3. runnyctl doctor   — the daemon comes up on its next restart\n",
		sysdaemon.ServiceUser, dir.LogsDir(), dir.ConfigPath())
	return nil
}

// uninstallDaemon removes the system LaunchDaemon. It leaves the service account
// and the home (config, key, artifacts) intact — see sysdaemon.Installer.Uninstall.
func uninstallDaemon(args []string) error {
	if err := parseNoFlagArgs("uninstall-daemon", args); err != nil {
		return err
	}
	if err := requireDarwinRoot("uninstall-daemon"); err != nil {
		return err
	}
	return sysdaemon.New(sysdaemon.DefaultConfig()).Uninstall(context.Background())
}

func parseNoFlagArgs(name string, args []string) error {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("%s takes no arguments (got %q)", name, fs.Arg(0))
	}
	return nil
}

func requireDarwinRoot(name string) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("%s installs a macOS system LaunchDaemon; it is macOS-only", name)
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("%s must run as root: re-run with `sudo runnyctl %s`", name, name)
	}
	return nil
}

// resolveOperator selects the operator account from the explicit --operator flag
// or $SUDO_USER. The explicit flag wins: the app's brokered install runs as root
// via osascript "with administrator privileges", which (unlike sudo) leaves
// SUDO_USER unset, so the operator can only travel by flag there; the headless
// `sudo runnyctl install-daemon` path passes no flag and resolves off SUDO_USER.
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

// checkNoPerUserAgent refuses install-daemon when the operator already has a
// per-user runnyd LaunchAgent registered: installing the system daemon over it
// would strand the per-user daemon orphaned behind it (clients resolve the system
// home, but the per-user agent keeps running and contends for the same VMs). The
// operator is validated authoritatively by Install (via user.Lookup), so a lookup
// miss here just skips the probe — Install then produces the canonical operator
// error rather than a duplicate. install-daemon runs as root, so probing the
// operator's gui/ domain is unrestricted.
func checkNoPerUserAgent(operator string) error {
	u, err := user.Lookup(operator)
	if err != nil {
		return nil
	}
	target := fmt.Sprintf("gui/%s/%s", u.Uid, sysdaemon.Label)
	refuse, warning := perUserAgentGuard(operator, target, launchd.Probe(context.Background(), launchd.ExecRunner, target))
	if warning != "" {
		fmt.Fprintln(os.Stderr, "warning:", warning)
	}
	return refuse
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
