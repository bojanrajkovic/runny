//go:build windows

package main

import (
	"fmt"
	"os/user"
	"strings"

	"golang.org/x/sys/windows"
)

// requireInstallPrivilege refuses install-daemon/uninstall-daemon unless the
// process token is elevated — the SCM install needs Administrator rights the
// same way darwin's needs euid 0.
func requireInstallPrivilege(name string) error {
	if !windows.GetCurrentProcessToken().IsElevated() {
		return fmt.Errorf("%s must run elevated: re-run `runnyctl %s` from an Administrator prompt", name, name)
	}
	return nil
}

// operatorFallback is the implicit operator source when --operator is not
// passed: the user running the elevated prompt. user.Current().Username is
// "DOMAIN\name" — both user.Lookup and icacls accept that form.
func operatorFallback() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	return u.Username
}

// refuseSystemOperator is the Windows analog of resolveOperator's hardcoded
// "root" refusal: NT AUTHORITY\SYSTEM is the account an elevated prompt runs
// as when launched non-interactively (a scheduled task, a remote PowerShell
// session), and an ACL grant to it is as pointless as one to root.
func refuseSystemOperator(operator string) error {
	if strings.EqualFold(operator, `NT AUTHORITY\SYSTEM`) {
		return fmt.Errorf("cannot grant the home's ACL to NT AUTHORITY\\SYSTEM — run install-daemon from your " +
			"normal elevated login, or pass --operator <domain>\\<user>")
	}
	return nil
}

// preflightPerUserAgent is a no-op: the app and its per-user LaunchAgent are
// macOS-only.
func preflightPerUserAgent(string) error { return nil }
