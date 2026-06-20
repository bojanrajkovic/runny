package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"

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
	return sysdaemon.New(cfg).Install(context.Background())
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
// Since the command runs as root, it is the account that invoked sudo.
func operatorAccount() (string, error) {
	op := os.Getenv("SUDO_USER")
	if op == "" || op == "root" {
		return "", fmt.Errorf("cannot determine the operator account: run via `sudo runnyctl install-daemon` " +
			"from your normal login so SUDO_USER is set (the home's ACL grants that account access)")
	}
	return op, nil
}
