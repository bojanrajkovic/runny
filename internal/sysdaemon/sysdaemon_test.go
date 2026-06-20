package sysdaemon

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/bojanrajkovic/runny/internal/home"
)

func TestPlist(t *testing.T) {
	cfg := DefaultConfig()
	cfg.RunnydPath = "/opt/homebrew/bin/runnyd"
	p := Plist(cfg)
	for _, want := range []string{
		"<key>Label</key>\n  <string>com.coderinserepeat.runnyd</string>",
		"<key>UserName</key>\n  <string>_runny</string>",
		"<string>/opt/homebrew/bin/runnyd</string>",
		"<key>KeepAlive</key>\n  <true/>",
		"<key>ProcessType</key>\n  <string>Standard</string>",
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

// The ACE strings are pinned: they are the exact grants validated by the PR4c
// spike on a real Mac. A change here silently re-opens the read gaps the spike
// closed (operator can't write, or the daemon can't read its own config/key).
func TestACEsPinned(t *testing.T) {
	wantOp := "user:alice allow list,add_file,search,delete,add_subdirectory,delete_child," +
		"readattr,writeattr,readextattr,writeextattr,readsecurity,read,write,append,execute," +
		"file_inherit,directory_inherit"
	if got := operatorACE("alice"); got != wantOp {
		t.Errorf("operatorACE drift:\n got %q\nwant %q", got, wantOp)
	}
	wantSvc := "user:_runny allow list,search,read,readattr,readextattr,readsecurity," +
		"file_inherit,directory_inherit"
	if got := serviceACE("_runny"); got != wantSvc {
		t.Errorf("serviceACE drift:\n got %q\nwant %q", got, wantSvc)
	}
}

func TestValidateOperatorName(t *testing.T) {
	for _, ok := range []string{"brajkovic", "_runner", "ci.bot", "a-b_c"} {
		if err := ValidateOperatorName(ok); err != nil {
			t.Errorf("ValidateOperatorName(%q) = %v, want nil", ok, err)
		}
	}
	// A name with spaces/commas would reshape the chmod ACE; reject it.
	for _, bad := range []string{"", "bob allow write,writesecurity", "a,b", "a b", "-x", "a/b", "a\nb"} {
		if err := ValidateOperatorName(bad); err == nil {
			t.Errorf("ValidateOperatorName(%q) = nil, want error", bad)
		}
	}
}

func TestFirstFreeID(t *testing.T) {
	if got, err := firstFreeID(map[int]bool{200: true, 201: true}); err != nil || got != 202 {
		t.Errorf("firstFreeID({200,201}) = %d, %v; want 202", got, err)
	}
	if got, err := firstFreeID(map[int]bool{}); err != nil || got != idRangeLo {
		t.Errorf("firstFreeID({}) = %d, %v; want %d", got, err, idRangeLo)
	}
	full := map[int]bool{}
	for id := idRangeLo; id <= idRangeHi; id++ {
		full[id] = true
	}
	if _, err := firstFreeID(full); err == nil {
		t.Error("firstFreeID(full range) should error")
	}
}

func TestParseTakenIDs(t *testing.T) {
	got := parseTakenIDs("root        0\n_runny      201\nbrajkovic   501\ngarbage\n")
	for _, id := range []int{0, 201, 501} {
		if !got[id] {
			t.Errorf("missing id %d", id)
		}
	}
	if len(got) != 3 {
		t.Errorf("got %d ids, want 3 (the unparseable line is skipped)", len(got))
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

// recordedRun is a fake Runner: it records every command and answers the few
// reads the installer makes (account existence + the dscl id lists).
type recordedRun struct {
	calls       [][]string
	readErr     error             // result of `dscl -read /Users/<svc>`
	readOut     string            // its stdout (the account's attributes), when readErr is nil
	listOut     map[string]string // attr -> `dscl -list` output
	printLoaded bool              // does `launchctl print system/<label>` find the job?
}

func (r *recordedRun) run(_ context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	joined := strings.Join(args, " ")
	switch {
	case name == "/usr/bin/dscl" && strings.Contains(joined, "-read /Users/"):
		return r.readOut, r.readErr
	case name == "/usr/bin/dscl" && strings.Contains(joined, "-list /Groups PrimaryGroupID"):
		return r.listOut["PrimaryGroupID"], nil
	case name == "/usr/bin/dscl" && strings.Contains(joined, "-list /Users UniqueID"):
		return r.listOut["UniqueID"], nil
	case name == "/bin/launchctl" && len(args) > 0 && args[0] == "print":
		if r.printLoaded {
			return "com.coderinserepeat.runnyd = { ... }", nil
		}
		return "Could not find service \"com.coderinserepeat.runnyd\" in domain for system",
			errors.New("launchctl print: exit 113")
	}
	return "", nil
}

func exactCall(calls [][]string, want ...string) bool {
	for _, c := range calls {
		if slices.Equal(c, want) {
			return true
		}
	}
	return false
}

func indexOfCall(calls [][]string, pred func([]string) bool) int {
	for i, c := range calls {
		if pred(c) {
			return i
		}
	}
	return -1
}

func newTestInstaller(r *recordedRun, wf func(string, []byte, os.FileMode) error) *Installer {
	cfg := DefaultConfig()
	cfg.Operator = "brajkovic"
	cfg.RunnydPath = "/opt/homebrew/bin/runnyd"
	return &Installer{cfg: cfg, run: r.run, writeFile: wf, log: func(string, ...any) {}}
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
	if !exactCall(r.calls, "/bin/chmod", "-R", "+a", operatorACE("brajkovic"), home.SystemHomeDir) {
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
	if !exactCall(r.calls, "/bin/launchctl", "bootstrap", "system", inst.cfg.PlistPath()) {
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

const validServiceAccountRead = "UniqueID: 250\nUserShell: /usr/bin/false\nNFSHomeDirectory: /var/empty\n"

func TestInstallReusesExistingAccount(t *testing.T) {
	// -read succeeds AND describes our dedicated service account → reuse it.
	r := &recordedRun{readErr: nil, readOut: validServiceAccountRead}
	inst := newTestInstaller(r, func(string, []byte, os.FileMode) error { return nil })
	if err := inst.Install(context.Background()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if i := indexOfCall(r.calls, func(c []string) bool {
		return len(c) >= 4 && c[0] == "/usr/bin/dscl" && c[2] == "-create" && strings.HasPrefix(c[3], "/Users/")
	}); i >= 0 {
		t.Errorf("must not re-create an existing account, but did: %v", r.calls[i])
	}
}

// A pre-existing _runny that is NOT our service account (a real login shell /
// login uid) must be refused, not adopted — adopting it would hand the daemon's
// home, key, and socket to the wrong principal.
func TestInstallRefusesForeignAccount(t *testing.T) {
	r := &recordedRun{readErr: nil, readOut: "UniqueID: 501\nUserShell: /bin/zsh\nNFSHomeDirectory: /Users/runny\n"}
	inst := newTestInstaller(r, func(string, []byte, os.FileMode) error { return nil })
	if err := inst.Install(context.Background()); err == nil {
		t.Fatal("Install must refuse a pre-existing non-service _runny account")
	}
	if i := indexOfCall(r.calls, func(c []string) bool {
		return len(c) >= 3 && c[0] == "/usr/bin/dscl" && c[2] == "-create"
	}); i >= 0 {
		t.Error("must not create/modify anything when refusing a foreign account")
	}
}

func TestVerifyServiceAccount(t *testing.T) {
	if err := verifyServiceAccount(validServiceAccountRead); err != nil {
		t.Errorf("valid service account rejected: %v", err)
	}
	for _, bad := range []string{
		"UniqueID: 250\nUserShell: /bin/zsh\nNFSHomeDirectory: /var/empty\n",       // login shell
		"UniqueID: 250\nUserShell: /usr/bin/false\nNFSHomeDirectory: /Users/x\n",   // real home
		"UniqueID: 501\nUserShell: /usr/bin/false\nNFSHomeDirectory: /var/empty\n", // login uid
		"UniqueID: 0\nUserShell: /usr/bin/false\nNFSHomeDirectory: /var/empty\n",   // root
		"UserShell: /usr/bin/false\nNFSHomeDirectory: /var/empty\n",                // no uid
	} {
		if err := verifyServiceAccount(bad); err == nil {
			t.Errorf("verifyServiceAccount accepted a non-service account:\n%s", bad)
		}
	}
}

func TestUninstallRemovesHomeKeepsAccount(t *testing.T) {
	r := &recordedRun{} // printLoaded=false → the job is gone after bootout
	inst := newTestInstaller(r, func(string, []byte, os.FileMode) error { return nil })
	if err := inst.Uninstall(context.Background()); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if !exactCall(r.calls, "/bin/launchctl", "bootout", "system/com.coderinserepeat.runnyd") {
		t.Error("uninstall must bootout the system job")
	}
	if !exactCall(r.calls, "/bin/launchctl", "print", "system/com.coderinserepeat.runnyd") {
		t.Error("uninstall must VERIFY the job is gone before removing anything")
	}
	if !exactCall(r.calls, "/bin/rm", "-f", inst.cfg.PlistPath()) {
		t.Error("uninstall must remove the plist")
	}
	if !exactCall(r.calls, "/bin/rm", "-rf", inst.cfg.HomeDir) {
		t.Error("uninstall must purge the home (else it poisons client resolution + leaves the key)")
	}
	if i := indexOfCall(r.calls, func(c []string) bool {
		return len(c) >= 3 && c[0] == "/usr/bin/dscl" && c[2] == "-delete"
	}); i >= 0 {
		t.Error("uninstall must NOT delete the service account (uid stability for reinstall)")
	}
}

// P2#2: a bootout that leaves the KeepAlive job loaded must NOT be reported as a
// successful uninstall over a still-running daemon.
func TestUninstallRefusesWhenJobStillLoaded(t *testing.T) {
	r := &recordedRun{printLoaded: true} // bootout didn't actually unload it
	inst := newTestInstaller(r, func(string, []byte, os.FileMode) error { return nil })
	if err := inst.Uninstall(context.Background()); err == nil {
		t.Fatal("Uninstall must refuse when the job is still loaded after bootout")
	}
	if exactCall(r.calls, "/bin/rm", "-f", inst.cfg.PlistPath()) {
		t.Error("must NOT remove the plist while the daemon is still running")
	}
	if exactCall(r.calls, "/bin/rm", "-rf", inst.cfg.HomeDir) {
		t.Error("must NOT purge the home while the daemon is still running")
	}
}

func TestInstallValidatesInputs(t *testing.T) {
	base := func() *Installer {
		return &Installer{
			cfg: DefaultConfig(), run: (&recordedRun{}).run,
			writeFile: func(string, []byte, os.FileMode) error { return nil }, log: func(string, ...any) {},
		}
	}
	noOp := base()
	noOp.cfg.RunnydPath = "/x/runnyd" // operator missing
	if err := noOp.Install(context.Background()); err == nil {
		t.Error("Install without an operator must error")
	}
	noRunnyd := base()
	noRunnyd.cfg.Operator = "brajkovic" // runnyd path missing
	if err := noRunnyd.Install(context.Background()); err == nil {
		t.Error("Install without a runnyd path must error")
	}
	badOp := base()
	badOp.cfg.Operator = "bob allow write,writesecurity" // not a plain username
	badOp.cfg.RunnydPath = "/x/runnyd"
	if err := badOp.Install(context.Background()); err == nil {
		t.Error("Install with a non-username operator must error before any mutation")
	}
}
