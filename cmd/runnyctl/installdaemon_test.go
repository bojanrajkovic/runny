package main

import (
	"strings"
	"testing"
)

// resolveOperator picks the operator account the system home's inheriting ACL
// will grant: the explicit --operator flag wins, else $SUDO_USER, and the name
// shape is validated (it is interpolated into a chmod ACE). The flag-wins rule is
// the whole point of PR5a: the app's brokered install runs as root via osascript
// "with administrator privileges", which — unlike sudo — leaves SUDO_USER unset,
// so the operator must travel by flag there. The headless `sudo` path passes no
// flag and still resolves off SUDO_USER. user.Lookup is the caller's, so this
// stays pure and host-independent.
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
		{name: "flag with a space — ACE-injection guard", flag: "alice bob", wantErrSub: "not a plain username"},
		{name: "flag with a comma — ACE-injection guard", flag: "alice,staff", wantErrSub: "not a plain username"},
		{name: "SUDO_USER with shell metachars is validated too", sudoUser: "a;rm -rf /", wantErrSub: "not a plain username"},
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
