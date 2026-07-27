package sysdaemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bojanrajkovic/runny/internal/home"
	"github.com/bojanrajkovic/runny/internal/opacl"
)

// testOperator is a local account that resolves via user.Lookup on any host,
// CI included. Install now requires the operator to be a real local user
// (user.Lookup), so a hardcoded developer name fails everywhere that account
// doesn't exist; the running user always resolves, with root as the fallback.
var testOperator = func() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return "root"
}()

// The ACE strings are pinned: they are the exact grants validated by the PR4c
// spike on a real Mac. A change here silently re-opens the read gaps the spike
// closed (operator can't write, or the daemon can't read its own config/key).
func TestACEsPinned(t *testing.T) {
	wantOp := "user:alice allow list,add_file,search,delete,add_subdirectory,delete_child," +
		"readattr,writeattr,readextattr,writeextattr,readsecurity,read,write,append,execute," +
		"file_inherit,directory_inherit"
	if got := opacl.OperatorACE("alice"); got != wantOp {
		t.Errorf("opacl.OperatorACE drift:\n got %q\nwant %q", got, wantOp)
	}
	wantSvc := "user:_runny allow list,search,read,readattr,readextattr,readsecurity," +
		"file_inherit,directory_inherit"
	if got := serviceACE("_runny"); got != wantSvc {
		t.Errorf("serviceACE drift:\n got %q\nwant %q", got, wantSvc)
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

// resolveRunnydPath's windows case is tested directly (not through
// ResolveRunnydPath, which hardcodes runtime.GOOS) so it is covered on every
// host, not just a windows build.
//
// It asserts the two things that decision actually makes -- the "runnyd.exe"
// suffix and the preserved directory -- rather than a whole path string.
// filepath's separator is baked in for the HOST at compile time regardless of
// the goos argument, so comparing a full path here passes off windows and fails
// on it: a test named for windows that only held when not run there.
func TestResolveRunnydPathWindows(t *testing.T) {
	in := filepath.Join(string(filepath.Separator)+"opt", "runny", "runnyctl")
	got := resolveRunnydPath(in, "windows")
	if want := "runnyd.exe"; filepath.Base(got) != want {
		t.Errorf("resolveRunnydPath(%q, windows) = %q, want basename %q", in, got, want)
	}
	if want := filepath.Dir(in); filepath.Dir(got) != want {
		t.Errorf("resolveRunnydPath(%q, windows) = %q, want dir %q", in, got, want)
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
	cfg := Config{Operator: testOperator, RunnydPath: "/opt/homebrew/bin/runnyd"}
	return &Installer{cfg: cfg, run: r.run, writeFile: wf, log: func(string, ...any) {}}
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
	if !exactCall(r.calls, "/bin/rm", "-f", PlistPath()) {
		t.Error("uninstall must remove the plist")
	}
	if !exactCall(r.calls, "/bin/rm", "-rf", home.SystemHomeDir) {
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
	if exactCall(r.calls, "/bin/rm", "-f", PlistPath()) {
		t.Error("must NOT remove the plist while the daemon is still running")
	}
	if exactCall(r.calls, "/bin/rm", "-rf", home.SystemHomeDir) {
		t.Error("must NOT purge the home while the daemon is still running")
	}
}

func TestInstallValidatesInputs(t *testing.T) {
	base := func() *Installer {
		return &Installer{
			cfg: Config{}, run: (&recordedRun{}).run,
			writeFile: func(string, []byte, os.FileMode) error { return nil }, log: func(string, ...any) {},
		}
	}
	noOp := base()
	noOp.cfg.RunnydPath = "/x/runnyd" // operator missing
	if err := noOp.Install(context.Background()); err == nil {
		t.Error("Install without an operator must error")
	}
	noRunnyd := base()
	noRunnyd.cfg.Operator = "_nonexistent" // RunnydPath="" fires before Lookup; no OS call needed
	if err := noRunnyd.Install(context.Background()); err == nil {
		t.Error("Install without a runnyd path must error")
	}
	badOp := base()
	badOp.cfg.Operator = "nonexistent-runny-test-user" // resolves to nobody → Lookup fails
	badOp.cfg.RunnydPath = "/x/runnyd"
	if err := badOp.Install(context.Background()); err == nil {
		t.Error("Install with an unresolvable operator must error before any mutation")
	}
}

// newStagingInstaller is newTestInstaller plus the account-creation reads a
// staged Install needs to reach stage() at all.
func newStagingInstaller(r *recordedRun, testConfig verdictTester) *Installer {
	r.readErr = errors.New("eDSRecordNotFound")
	r.listOut = map[string]string{"PrimaryGroupID": "staff 20\n", "UniqueID": "root 0\n"}
	inst := newTestInstaller(r, func(string, []byte, os.FileMode) error { return nil })
	inst.testConfig = testConfig
	return inst.WithStage(StagePlan{Config: []byte("pools: []\n")})
}

// stage() must bound the -test-config exec itself: Install runs with
// context.Background() (installdaemon.go never sets a deadline), so if stage
// didn't wrap the call, a hung staged binary would block install forever —
// exactly what commandTimeout exists to prevent for every other privileged
// step in this file.
func TestStageBoundsTestConfigCall(t *testing.T) {
	r := &recordedRun{}
	var sawDeadline bool
	inst := newStagingInstaller(r, func(ctx context.Context, _, _ string) (home.Verdict, error) {
		_, sawDeadline = ctx.Deadline()
		return home.Verdict{Status: home.VerdictOK}, nil
	})
	if err := inst.Install(context.Background()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !sawDeadline {
		t.Error("testConfig must be called with a bounded context, not the caller's undeadlined ctx")
	}
}

// A warn-tier verdict must not be silently discarded just because -test-config
// exits 0 — the fresh-install moment is the one chance the operator has to see
// it before the daemon comes up on it.
func TestStageSurfacesWarningsAndProceeds(t *testing.T) {
	r := &recordedRun{}
	var logs []string
	inst := newStagingInstaller(r, func(context.Context, string, string) (home.Verdict, error) {
		return home.Verdict{Status: home.VerdictWarn, Warnings: []home.Warning{
			{Kind: home.WarnResourceOvercommit, Message: "cpu oversubscribed"},
		}}, nil
	})
	inst.log = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	if err := inst.Install(context.Background()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !slices.ContainsFunc(logs, func(l string) bool { return strings.Contains(l, "cpu oversubscribed") }) {
		t.Errorf("warn-tier verdict's warning was not logged; got %v", logs)
	}
	if !exactCall(r.calls, "/bin/launchctl", "bootstrap", "system", PlistPath()) {
		t.Error("a warn-tier verdict must still let install proceed to bootstrap")
	}
}

// An error-tier (or any status this runnyctl/runnyd version doesn't recognize)
// must still block install and leave the home in place without bootstrapping —
// exactly what gating on the exit code alone already gave us for errors;
// parsing the JSON must not regress that, and an unrecognized status must fail
// closed rather than being treated as an implicit ok (matching decideUpgrade
// and edit-config's gates on the same contract).
func TestStageFailsOnBlockingVerdict(t *testing.T) {
	cases := map[string]home.Verdict{
		"error tier":          {Status: home.VerdictError, Errors: []string{"pools[0].github.app_id: required"}},
		"unrecognized status": {Status: "future-status"},
	}
	for name, verdict := range cases {
		t.Run(name, func(t *testing.T) {
			r := &recordedRun{}
			inst := newStagingInstaller(r, func(context.Context, string, string) (home.Verdict, error) {
				return verdict, nil
			})
			err := inst.Install(context.Background())
			if err == nil {
				t.Fatal("Install must refuse a blocking verdict")
			}
			// The status must appear in the message even when Errors is empty (the
			// "unrecognized status" case) — otherwise the operator sees no reason
			// for the refusal.
			if !strings.Contains(err.Error(), verdict.Status) {
				t.Errorf("error must name the verdict's status %q, got: %v", verdict.Status, err)
			}
			if exactCall(r.calls, "/bin/launchctl", "bootstrap", "system", PlistPath()) {
				t.Error("must not bootstrap over a staged config that failed validation")
			}
		})
	}
}
