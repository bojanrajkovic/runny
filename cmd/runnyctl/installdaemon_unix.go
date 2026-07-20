//go:build !windows

package main

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"runtime"

	"github.com/bojanrajkovic/runny/internal/launchd"
	"github.com/bojanrajkovic/runny/internal/sysdaemon"
)

// requireInstallPrivilege refuses install-daemon/uninstall-daemon unless
// running as root on darwin, the only non-Windows platform the LaunchDaemon
// installer supports — every other GOOS gets a platform-neutral unsupported
// error rather than attempting a doomed install.
func requireInstallPrivilege(name string) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("%s installs a system daemon; it is supported on macOS and Windows only", name)
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("%s must run as root: re-run with `sudo runnyctl %s`", name, name)
	}
	return nil
}

// operatorFallback is the implicit operator source when --operator is not
// passed: the human who ran sudo.
func operatorFallback() string { return os.Getenv("SUDO_USER") }

// refuseSystemOperator is Windows-only (NT AUTHORITY\SYSTEM); root is already
// refused inside resolveOperator on every platform.
func refuseSystemOperator(string) error { return nil }

// preflightPerUserAgent refuses install-daemon when the operator already has a
// per-user runnyd LaunchAgent registered: installing the system daemon over it
// would strand the per-user daemon orphaned behind it (clients resolve the system
// home, but the per-user agent keeps running and contends for the same VMs). The
// operator is validated authoritatively by Install (via user.Lookup), so a lookup
// miss here just skips the probe — Install then produces the canonical operator
// error rather than a duplicate. install-daemon runs as root, so probing the
// operator's gui/ domain is unrestricted.
func preflightPerUserAgent(operator string) error {
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
