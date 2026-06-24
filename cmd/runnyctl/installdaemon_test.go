package main

import (
	"strings"
	"testing"

	"github.com/bojanrajkovic/runny/internal/launchd"
)

// resolveOperator picks the operator account the system home's inheriting ACL
// will grant: the explicit --operator flag wins, else $SUDO_USER. The flag-wins
// rule is the whole point of PR5a: the app's brokered install runs as root via
// osascript "with administrator privileges", which — unlike sudo — leaves
// SUDO_USER unset, so the operator must travel by flag there. The headless
// `sudo` path passes no flag and still resolves off SUDO_USER. The local-user
// check lives in Install(), so this stays pure and host-independent.
func TestResolveOperator(t *testing.T) {
	cases := []struct {
		name       string
		flag       string
		sudoUser   string
		want       string
		wantErrSub string // non-empty => expect an error containing this substring
	}{
		{name: "flag only, no SUDO_USER (the brokered-install case)", flag: "alice", want: "alice"},
		{name: "flag wins over SUDO_USER", flag: "alice", sudoUser: "bob", want: "alice"},
		{name: "SUDO_USER fallback (the headless sudo path)", sudoUser: "bob", want: "bob"},
		{name: "neither set", wantErrSub: "cannot determine the operator account"},
		{name: "SUDO_USER is root (plain sudo, not from a login)", sudoUser: "root", wantErrSub: "cannot determine"},
		{name: "explicit --operator root is refused (ACL to root is pointless)", flag: "root", wantErrSub: "cannot determine"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveOperator(tc.flag, tc.sudoUser)
			if tc.wantErrSub != "" {
				if err == nil {
					t.Fatalf("expected an error containing %q, got nil (result %q)", tc.wantErrSub, got)
				}
				if !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("resolveOperator(%q, %q) = %q, want %q", tc.flag, tc.sudoUser, got, tc.want)
			}
		})
	}
}

// install-daemon takes --operator now; an unknown flag must still be named (not
// swallowed) and a stray positional refused — both before any privileged step, so
// the assertions are host-independent (they never reach the darwin/root gate).
func TestInstallDaemonRejectsBadArgs(t *testing.T) {
	if err := installDaemon([]string{"-bogus"}); err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Errorf("an unknown flag should be surfaced by name; got %v", err)
	}
	if err := installDaemon([]string{"extra"}); err == nil || !strings.Contains(err.Error(), "positional") {
		t.Errorf("a stray positional should be refused; got %v", err)
	}
}

// install-daemon refuses to install a system daemon over a registered per-user
// agent: it would strand the per-user daemon orphaned behind the system one. The
// pure guard decides from the operator + the probe result, so it is host- and
// launchd-independent.
func TestPerUserAgentGuard(t *testing.T) {
	const op = "alice"
	const target = "gui/501/com.coderinserepeat.runnyd"

	// A registered per-user agent: refuse, naming the operator, the target, and the
	// bootout remedy. No warning (a refusal is not a proceed).
	refuse, warning := perUserAgentGuard(op, target, launchd.Registered)
	if refuse == nil {
		t.Fatal("a registered per-user agent must refuse the install")
	}
	for _, want := range []string{op, target, "bootout"} {
		if !strings.Contains(refuse.Error(), want) {
			t.Errorf("refusal %q must name %q", refuse.Error(), want)
		}
	}
	if warning != "" {
		t.Errorf("a refusal carries no warning, got %q", warning)
	}

	// No per-user agent: proceed cleanly.
	if refuse, warning := perUserAgentGuard(op, target, launchd.NotRegistered); refuse != nil || warning != "" {
		t.Errorf("an absent agent must proceed cleanly, got refuse=%v warning=%q", refuse, warning)
	}

	// Inconclusive probe: proceed, but warn — don't block a privileged install on a
	// transient launchctl failure; the doctor check and the flock are the safety nets.
	refuse, warning = perUserAgentGuard(op, target, launchd.Indeterminate)
	if refuse != nil {
		t.Errorf("an inconclusive probe must NOT block install, got %v", refuse)
	}
	if !strings.Contains(warning, op) {
		t.Errorf("an inconclusive probe must warn (naming the operator), got %q", warning)
	}
}
