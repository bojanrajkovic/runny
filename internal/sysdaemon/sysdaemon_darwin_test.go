package sysdaemon

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/bojanrajkovic/runny/internal/home"
	"github.com/bojanrajkovic/runny/internal/opacl"
)

// These assert macOS installer ARTIFACTS -- a LaunchDaemon plist, its path under
// /Library/LaunchDaemons, and the dscl/launchctl/chmod plan that installs it.
// They are darwin-only because the artifacts are: ResolveRunnydPath hardcodes
// runtime.GOOS, so on windows it correctly returns a windows path with a .exe
// suffix, and asserting mac shapes there tests the host rather than the code.
//
// The windows counterparts live in scm_windows_test.go, which covers the SCM
// install/uninstall plan in more depth than this file covers launchd. Splitting
// by filename (_darwin_test.go) rather than a build tag keeps each platform's
// suite running on the platform that produces its artifacts.

func TestPlist(t *testing.T) {
	cfg := Config{RunnydPath: "/opt/homebrew/bin/runnyd"}
	p := Plist(cfg)
	// Check keys and values independently — format-agnostic so the test doesn't
	// pin howett.net/plist's internal whitespace.
	for _, want := range []string{
		"<key>Label</key>",
		"<string>com.coderinserepeat.runnyd</string>",
		"<key>UserName</key>",
		"<string>_runny</string>",
		"<string>/opt/homebrew/bin/runnyd</string>",
		"<key>KeepAlive</key>",
		"<true/>",
		"<key>ProcessType</key>",
		"<string>Standard</string>",
		"<string>/Library/Application Support/runny/logs/launchd.out.log</string>",
		"<string>/Library/Application Support/runny/logs/launchd.err.log</string>",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("plist missing %q\n---\n%s", want, p)
		}
	}
	// A system daemon must NOT carry the per-user agent's Interactive type.
	if strings.Contains(p, "<string>Interactive</string>") {
		t.Error("system plist must not use ProcessType Interactive (no GUI session)")
	}
}

func TestResolveRunnydPath(t *testing.T) {
	cases := map[string]string{
		"/opt/homebrew/bin/runnyctl":                      "/opt/homebrew/bin/runnyd",
		"/Applications/Runny.app/Contents/MacOS/runnyctl": "/Applications/Runny.app/Contents/MacOS/runnyd",
	}
	for in, want := range cases {
		if got := ResolveRunnydPath(in); got != want {
			t.Errorf("ResolveRunnydPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestInstallPlan(t *testing.T) {
	r := &recordedRun{
		readErr: errors.New("eDSRecordNotFound"),
		listOut: map[string]string{
			"PrimaryGroupID": "staff 20\nadmin 80\n",
			"UniqueID":       "root 0\nbrajkovic 501\n",
		},
	}
	var wrotePath string
	var wroteData []byte
	var wroteMode os.FileMode
	inst := newTestInstaller(r, func(p string, d []byte, m os.FileMode) error {
		wrotePath, wroteData, wroteMode = p, d, m
		return nil
	})
	if err := inst.Install(context.Background()); err != nil {
		t.Fatalf("Install: %v", err)
	}

	// Account: hidden, no-home, no-shell, with the first free uid (200).
	for _, want := range [][]string{
		{"/usr/bin/dscl", ".", "-create", "/Users/_runny", "UniqueID", "200"},
		{"/usr/bin/dscl", ".", "-create", "/Users/_runny", "NFSHomeDirectory", "/var/empty"},
		{"/usr/bin/dscl", ".", "-create", "/Users/_runny", "IsHidden", "1"},
		{"/usr/bin/dscl", ".", "-create", "/Users/_runny", "UserShell", "/usr/bin/false"},
	} {
		if !exactCall(r.calls, want...) {
			t.Errorf("missing account step: %v", want)
		}
	}

	// Home: 0700 + the dual inheriting ACL.
	if !exactCall(r.calls, "/bin/chmod", "0700", home.SystemHomeDir) {
		t.Error("missing chmod 0700 on the home")
	}
	if !exactCall(r.calls, "/bin/chmod", "-R", "+a", opacl.OperatorACE(testOperator), home.SystemHomeDir) {
		t.Error("missing operator ACE (recursive)")
	}
	if !exactCall(r.calls, "/bin/chmod", "-R", "+a", serviceACE("_runny"), home.SystemHomeDir) {
		t.Error("missing service ACE (recursive)")
	}

	// Ordering: logs/ must be created AFTER the home ACL so it inherits the ACEs.
	lastACL := -1
	for i, c := range r.calls {
		if len(c) >= 3 && c[0] == "/bin/chmod" && c[2] == "+a" {
			lastACL = i
		}
	}
	logsIdx := indexOfCall(r.calls, func(c []string) bool {
		return len(c) == 3 && c[0] == "/bin/mkdir" && c[2] == home.SystemHomeDir+"/logs"
	})
	if logsIdx < 0 || lastACL < 0 || logsIdx < lastACL {
		t.Errorf("logs/ (idx %d) must be created after the home ACL (idx %d) so it inherits", logsIdx, lastACL)
	}

	// launchctl: bootstrap into system/ then enable.
	if !exactCall(r.calls, "/bin/launchctl", "bootstrap", "system", PlistPath()) {
		t.Error("missing launchctl bootstrap system")
	}
	if !exactCall(r.calls, "/bin/launchctl", "enable", "system/com.coderinserepeat.runnyd") {
		t.Error("missing launchctl enable")
	}

	// Plist: right path, mode, and contents.
	if wrotePath != "/Library/LaunchDaemons/com.coderinserepeat.runnyd.plist" {
		t.Errorf("plist path = %q", wrotePath)
	}
	if wroteMode != 0o644 {
		t.Errorf("plist mode = %v, want 0644", wroteMode)
	}
	if !strings.Contains(string(wroteData), "<string>/opt/homebrew/bin/runnyd</string>") {
		t.Error("plist missing the runnyd path")
	}
}
