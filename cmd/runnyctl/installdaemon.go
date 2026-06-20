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
	"github.com/bojanrajkovic/runny/internal/sysdaemon"
)

// installDaemon installs runnyd as a non-root system LaunchDaemon. It is a
// privileged local command (run via `sudo runnyctl install-daemon`), never a
// daemon RPC — the daemon does not exist yet. The plist points at the runnyd
// sibling of this runnyctl, and the inheriting ACL grants the operator account
// (the human who ran sudo, from SUDO_USER).
func installDaemon(args []string) error {
	if err := parseNoFlagArgs("install-daemon", args); err != nil {
		return err
	}
	if err := requireDarwinRoot("install-daemon"); err != nil {
		return err
	}
	operator, err := operatorAccount()
	if err != nil {
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

// operatorAccount is the human operator the inheriting ACL grants access to.
// Since the command runs as root, it is the account that invoked sudo. The name
// is validated (it is interpolated into a security-critical ACL ACE) and checked
// to resolve to a real local user BEFORE any privileged step runs, so a bad
// value fails clean rather than half-installing.
func operatorAccount() (string, error) {
	op := os.Getenv("SUDO_USER")
	if op == "" || op == "root" {
		return "", fmt.Errorf("cannot determine the operator account: run via `sudo runnyctl install-daemon` " +
			"from your normal login so SUDO_USER is set (the home's ACL grants that account access)")
	}
	if err := sysdaemon.ValidateOperatorName(op); err != nil {
		return "", err
	}
	if _, err := user.Lookup(op); err != nil {
		return "", fmt.Errorf("operator account %q does not resolve to a local user: %w", op, err)
	}
	return op, nil
}
