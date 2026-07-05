package main

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bojanrajkovic/runny/internal/launchd"
)

// resolveOperator picks the operator account the system home's inheriting ACL
// will grant: the explicit --operator flag wins, else $SUDO_USER. The flag-wins
// rule covers a root invocation without sudo (e.g. CI, or `su root`), which leaves
// SUDO_USER unset, so the operator must travel by flag there. The headless `sudo`
// path passes no flag and still resolves off SUDO_USER. The local-user check
// lives in Install(), so this stays pure and host-independent.
func TestResolveOperator(t *testing.T) {
	cases := []struct {
		name       string
		flag       string
		sudoUser   string
		want       string
		wantErrSub string // non-empty => expect an error containing this substring
	}{
		{name: "flag only, no SUDO_USER (root without sudo)", flag: "alice", want: "alice"},
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

// currentUsername resolves the running test user — the one account
// planStageFromFile can always look up via user.Lookup on any host, CI included.
func currentUsername(t *testing.T) string {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Skipf("user.Current: %v", err)
	}
	return u.Username
}

func TestPlanStageFromFileResolvesAndPreflights(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "runner-app.pem")
	if err := os.WriteFile(keyPath, []byte("fake key"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.yaml")
	config := "pools:\n" +
		"  - name: mac\n" +
		"    os: darwin\n" +
		"    image: ghcr.io/example/image:latest\n" +
		"    target:\n" +
		"      org: my-org\n" +
		"    github:\n" +
		"      app_id: 1\n" +
		"      private_key_path: " + keyPath + "\n"
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	plan, err := planStageFromFile(configPath, "/Library/Application Support/runny", currentUsername(t))
	if err != nil {
		t.Fatalf("planStageFromFile: %v", err)
	}
	if len(plan.Keys) != 1 || plan.Keys[0].Src != keyPath {
		t.Fatalf("plan.Keys = %+v", plan.Keys)
	}
	if !strings.Contains(string(plan.Config), "/Library/Application Support/runny/runner-app.pem") {
		t.Errorf("config not rewritten to the in-home dest: %s", plan.Config)
	}
}

// A pool naming a key file that doesn't exist must abort before anything is
// staged — never a partial copy the operator has to notice and clean up.
func TestPlanStageFromFileMissingKeyAborts(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	config := "pools:\n" +
		"  - name: mac\n" +
		"    os: darwin\n" +
		"    image: ghcr.io/example/image:latest\n" +
		"    target:\n" +
		"      org: my-org\n" +
		"    github:\n" +
		"      app_id: 1\n" +
		"      private_key_path: " + filepath.Join(dir, "missing.pem") + "\n"
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := planStageFromFile(configPath, "/Library/Application Support/runny", currentUsername(t)); err == nil {
		t.Fatal("planStageFromFile must refuse when a pool's private key is missing")
	}
}

func TestPlanStageFromFileRejectsUnparseableConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("not: valid: yaml: at: all:\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := planStageFromFile(configPath, "/Library/Application Support/runny", currentUsername(t)); err == nil {
		t.Fatal("planStageFromFile must refuse an unparseable config")
	}
}
