package guest

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/bojanrajkovic/runny/internal/bounded"
	"github.com/bojanrajkovic/runny/internal/home"
	"github.com/bojanrajkovic/runny/internal/sshx"
)

// perOS picks between darwinVal and linuxVal — the only two POSIX provision
// shapes — and errors on anything else, not a silent darwin fallback: a
// third guest OS existing makes the old fallthrough a no-silent-failure
// violation, not a convenience.
func TestPerOSDispatchesTwoWays(t *testing.T) {
	cases := []struct {
		goos string
		want string
	}{
		{home.OSDarwin, "darwin"},
		{home.OSLinux, "linux"},
	}
	for _, tc := range cases {
		got, err := perOS(tc.goos, "darwin", "linux")
		if err != nil {
			t.Errorf("perOS(%q, ...) unexpected error: %v", tc.goos, err)
		}
		if got != tc.want {
			t.Errorf("perOS(%q, ...) = %q, want %q", tc.goos, got, tc.want)
		}
	}
}

// Every windows call site dispatches to its own windows path before perOS
// ever runs, so perOS itself doesn't know a windows value — an OS name
// reaching it that isn't darwin or linux (windows included) is a loud error.
func TestPerOSUnknownOSIsLoudError(t *testing.T) {
	for _, goos := range []string{"", "plan9", "freebsd", home.OSWindows} {
		if _, err := perOS(goos, "darwin", "linux"); err == nil {
			t.Errorf("perOS(%q, ...) succeeded, want a loud error", goos)
		}
	}
}

// The darwin runner launches over a non-login SSH exec, whose PATH lacks
// /usr/local/bin; the provision script must rebuild a login PATH so job steps
// find tools that pkg installers symlink there. Regression guard for the
// "aws: command not found" right after a successful install.
func TestProvisionScriptDarwinPrimesPATH(t *testing.T) {
	if !strings.Contains(provisionScriptDarwin, "/usr/libexec/path_helper") {
		t.Error("darwin provision script must rebuild PATH via path_helper before launching the runner")
	}
}

// Pin the provision-clock tripwire in both scripts so a refactor can't
// silently drop it.
func TestProvisionScriptsReportProvisionClock(t *testing.T) {
	for _, s := range []struct {
		name, script string
	}{
		{"darwin", provisionScriptDarwin},
		{"linux", provisionScriptLinux},
		{"linux (windows host)", provisionScriptLinuxPushed},
	} {
		if !strings.Contains(s.script, `echo "runny: provision-clock $(date -u +%Y-%m-%dT%H:%M:%SZ)"`) {
			t.Errorf("%s provision script must report the guest clock (runny: provision-clock ...)", s.name)
		}
	}
}

// The JIT config is a secret: it must reach run.sh over stdin (`$(cat)`), never
// be interpolated into the command string, where x/crypto would fold it into
// the exec error on a server-side reject and leak it to cycle.json and the
// gRPC surface. Guard both: the scripts read the JIT from stdin, and they carry
// no `%s` format verb that StartRunner could fill with the blob.
func TestProvisionScriptsKeepJITOutOfCommand(t *testing.T) {
	for _, s := range []struct {
		name, script string
	}{
		{"darwin", provisionScriptDarwin},
		{"linux", provisionScriptLinux},
		{"linux (windows host)", provisionScriptLinuxPushed},
	} {
		if !strings.Contains(s.script, `--jitconfig "$(cat)"`) {
			t.Errorf("%s provision script must read the JIT from stdin via $(cat)", s.name)
		}
		if strings.Contains(s.script, "%s") {
			t.Errorf("%s provision script must not carry a %%s verb — the JIT must never be formatted into the command", s.name)
		}
	}
}

// provisionScript stages the EXACT resolved tarball, not a lexical glob of the
// shared cache — so the staged runner can't drift from the version recorded as
// the cycle's RunnerVersion when the share briefly holds more than one.
func TestProvisionScriptStagesExactTarball(t *testing.T) {
	for _, tc := range []struct{ goos, name string }{
		{"darwin", "actions-runner-osx-arm64-2.320.0.tar.gz"},
		{"linux", "actions-runner-linux-arm64-2.320.0.tar.gz"},
	} {
		script, err := provisionScript(tc.goos, tc.name, nil, nil, false)
		if err != nil {
			t.Fatalf("%s: provisionScript(%q): %v", tc.goos, tc.name, err)
		}
		if !strings.Contains(script, `TARBALL="$CACHE/`+tc.name+`"`) {
			t.Errorf("%s: script does not stage the exact tarball %q:\n%s", tc.goos, tc.name, script)
		}
		if strings.Contains(script, "ls ") || strings.Contains(script, "head -1") {
			t.Errorf("%s: script still globs the cache instead of staging by name", tc.goos)
		}
		if strings.Contains(script, runnerTarballPlaceholder) {
			t.Errorf("%s: script left the %s placeholder unsubstituted", tc.goos, runnerTarballPlaceholder)
		}
		if !strings.Contains(script, `--jitconfig "$(cat)"`) {
			t.Errorf("%s: script must still read the JIT from stdin", tc.goos)
		}
	}
}

// When the caller's needsPush is true (the boot backend's own
// vm.Machine.NeedsRunnerPush(), threaded through by the state machine), a
// linux guest's provision script must skip the virtiofs mount entirely
// (schema 2.1 has no working live share device for a stock Linux guest on a
// bare compute system) and read the tarball from where PushRunnerTarball
// stages it instead.
func TestProvisionScriptLinuxSkipsMountWhenPushed(t *testing.T) {
	const tarball = "actions-runner-linux-amd64-2.320.0.tar.gz"

	script, err := provisionScript("linux", tarball, nil, nil, true)
	if err != nil {
		t.Fatalf("provisionScript: %v", err)
	}
	if strings.Contains(script, "mount") {
		t.Errorf("pushed linux script must not attempt any mount:\n%s", script)
	}
	if !strings.Contains(script, `CACHE="$HOME/`+runnerPushCacheDir+`"`) {
		t.Errorf("pushed linux script must read from $HOME/%s, got:\n%s", runnerPushCacheDir, script)
	}
	if !strings.Contains(script, `TARBALL="$CACHE/`+tarball+`"`) {
		t.Errorf("pushed linux script must still stage the exact tarball %q:\n%s", tarball, script)
	}

	script, err = provisionScript("linux", tarball, nil, nil, false)
	if err != nil {
		t.Fatalf("provisionScript: %v", err)
	}
	if !strings.Contains(script, "mount -t virtiofs") {
		t.Errorf("unpushed linux script must still mount virtiofs, got:\n%s", script)
	}

	// The darwin GUEST script is unaffected by needsPush either way.
	const darwinTarball = "actions-runner-osx-arm64-2.320.0.tar.gz"
	darwinScript, err := provisionScript("darwin", darwinTarball, nil, nil, true)
	if err != nil {
		t.Fatalf("provisionScript: %v", err)
	}
	want := strings.ReplaceAll(provisionScriptDarwin, runnerTarballPlaceholder, darwinTarball)
	if darwinScript != want {
		t.Errorf("darwin guest script must be unaffected by needsPush, got:\n%s\nwant:\n%s", darwinScript, want)
	}
}

// A tarball name that does not match the strict pattern (it crosses into a
// shell command string) is refused loudly, not staged. Empty, shell
// metacharacters, path separators, and a missing .tar.gz all fail.
func TestProvisionScriptRejectsBadName(t *testing.T) {
	for _, bad := range []string{
		"",
		"actions-runner-osx-arm64-2.320.0.tar.gz; rm -rf /",
		"../../etc/passwd",
		"actions-runner-osx-arm64-$(whoami).tar.gz",
		"actions-runner-osx-arm64-2.320.0", // no .tar.gz
		"foo bar.tar.gz",
	} {
		if _, err := provisionScript("darwin", bad, nil, nil, false); err == nil {
			t.Errorf("provisionScript accepted an unsafe tarball name %q", bad)
		}
	}
}

// provisionScript is POSIX-only: a windows guest's launch is StartRunner's
// startRunnerWindows hand-off, not a single exec'd script, so provisionScript
// must refuse windows loudly rather than silently render an empty script.
func TestProvisionScriptRejectsWindows(t *testing.T) {
	if _, err := provisionScript("windows", "actions-runner-win-x64-2.320.0.zip", nil, nil, true); err == nil {
		t.Error("provisionScript accepted goos=windows, want a loud refusal")
	}
}

// runnerAssetRE selects the tarball charset for darwin/linux and the zip
// charset for windows; a windows-shaped tarball name and vice versa are both
// rejected, alongside the usual shell-metacharacter cases.
func TestRunnerAssetRENamePerOS(t *testing.T) {
	if !runnerAssetRE("windows").MatchString("actions-runner-win-x64-2.320.0.zip") {
		t.Error("windows asset regex rejected a well-formed .zip name")
	}
	for _, bad := range []string{
		"actions-runner-win-x64-2.320.0.tar.gz", // tar.gz on windows
		"actions-runner-win-x64-$(whoami).zip",
		"actions-runner-win-x64-2.320.0.zip; rm -rf /",
		"foo bar.zip",
		"",
	} {
		if runnerAssetRE("windows").MatchString(bad) {
			t.Errorf("windows asset regex accepted unsafe name %q", bad)
		}
	}
	if runnerAssetRE("linux").MatchString("actions-runner-linux-amd64-2.320.0.zip") {
		t.Error("linux asset regex accepted a .zip name")
	}
}

// guestEnvExports renders sorted, single-quote-escaped `export` lines; nil/empty
// renders nothing (the no-op that keeps provisioning byte-identical).
func TestGuestEnvExports(t *testing.T) {
	if got := guestEnvExports(nil); got != "" {
		t.Errorf("guestEnvExports(nil) = %q, want empty", got)
	}
	if got := guestEnvExports(map[string]string{}); got != "" {
		t.Errorf("guestEnvExports(empty) = %q, want empty", got)
	}
	// Deterministic key order, regardless of map iteration order.
	got := guestEnvExports(map[string]string{"B_VAR": "2", "A_VAR": "1"})
	want := "export A_VAR='1'\nexport B_VAR='2'\n"
	if got != want {
		t.Errorf("guestEnvExports sorted = %q, want %q", got, want)
	}
	// A single quote in the value is POSIX-escaped ('\'') so the value is inert.
	if got := guestEnvExports(map[string]string{"K": "a'b"}); got != `export K='a'\''b'`+"\n" {
		t.Errorf("guestEnvExports escaping = %q", got)
	}
}

// A pool's guest_env is exported into the guest shell BEFORE the runner launches,
// so run.sh and every job step inherit it.
func TestProvisionScriptInjectsGuestEnv(t *testing.T) {
	const tarball = "actions-runner-osx-arm64-2.320.0.tar.gz"
	script, err := provisionScript("darwin", tarball, map[string]string{
		"HTTPS_PROXY": "socks5h://192.168.64.1:1080",
	}, nil, false)
	if err != nil {
		t.Fatalf("provisionScript: %v", err)
	}
	const want = `export HTTPS_PROXY='socks5h://192.168.64.1:1080'`
	ei, ri := strings.Index(script, want), strings.Index(script, "exec ./run.sh")
	if ei < 0 {
		t.Fatalf("script missing exported var:\n%s", script)
	}
	if ei > ri {
		t.Errorf("guest_env exported AFTER the runner launches (%d > %d):\n%s", ei, ri, script)
	}
}

// No guest_env is byte-for-byte the pre-feature script: the injection must be a
// pure no-op, never a stray blank line or export.
func TestProvisionScriptNoGuestEnvIsInert(t *testing.T) {
	const tarball = "actions-runner-osx-arm64-2.320.0.tar.gz"
	script, err := provisionScript("darwin", tarball, nil, nil, false)
	if err != nil {
		t.Fatalf("provisionScript: %v", err)
	}
	if strings.Contains(script, "export ") {
		t.Errorf("nil guest_env produced an export line:\n%s", script)
	}
}

// guestSetupBlock renders newline-joined commands verbatim; nil/empty renders
// nothing (the no-op that keeps provisioning byte-identical).
func TestGuestSetupBlock(t *testing.T) {
	if got := guestSetupBlock(nil); got != "" {
		t.Errorf("guestSetupBlock(nil) = %q, want empty", got)
	}
	if got := guestSetupBlock([]string{}); got != "" {
		t.Errorf("guestSetupBlock(empty) = %q, want empty", got)
	}
	got := guestSetupBlock([]string{"defaults write com.apple.dt.Xcode IDEPackageSupportUseBuiltinSCM -bool YES", "sudo networksetup -setwebproxy Wi-Fi 192.168.64.1 1080"})
	want := "defaults write com.apple.dt.Xcode IDEPackageSupportUseBuiltinSCM -bool YES\nsudo networksetup -setwebproxy Wi-Fi 192.168.64.1 1080\n"
	if got != want {
		t.Errorf("guestSetupBlock = %q, want %q", got, want)
	}
}

// A pool's guest_setup runs after the guest_env exports and before the runner
// launches, so hooks can rely on guest_env already being in scope.
func TestProvisionScriptInjectsGuestSetupAfterGuestEnv(t *testing.T) {
	const tarball = "actions-runner-osx-arm64-2.320.0.tar.gz"
	script, err := provisionScript("darwin", tarball,
		map[string]string{"HTTPS_PROXY": "socks5h://192.168.64.1:1080"},
		[]string{"defaults write com.apple.dt.Xcode IDEPackageSupportUseBuiltinSCM -bool YES"}, false)
	if err != nil {
		t.Fatalf("provisionScript: %v", err)
	}
	envIdx := strings.Index(script, "export HTTPS_PROXY=")
	setupIdx := strings.Index(script, "defaults write com.apple.dt.Xcode")
	runIdx := strings.Index(script, "exec ./run.sh")
	if envIdx < 0 || setupIdx < 0 || runIdx < 0 {
		t.Fatalf("script missing expected pieces:\n%s", script)
	}
	if !(envIdx < setupIdx && setupIdx < runIdx) {
		t.Errorf("wrong ordering: env=%d setup=%d run=%d, want env < setup < run:\n%s", envIdx, setupIdx, runIdx, script)
	}
}

// No guest_setup is byte-for-byte the pre-feature script: the injection must be
// a pure no-op.
func TestProvisionScriptNoGuestSetupIsInert(t *testing.T) {
	const tarball = "actions-runner-osx-arm64-2.320.0.tar.gz"
	without, err := provisionScript("darwin", tarball, nil, nil, false)
	if err != nil {
		t.Fatalf("provisionScript: %v", err)
	}
	withEmpty, err := provisionScript("darwin", tarball, nil, []string{}, false)
	if err != nil {
		t.Fatalf("provisionScript: %v", err)
	}
	if without != withEmpty {
		t.Errorf("empty guest_setup changed the script:\nwithout=%q\nwithEmpty=%q", without, withEmpty)
	}
}

// The runner-launch line is the injection anchor for guest_env; pin it in both
// scripts so a refactor can't silently move the export block out of the runner's
// environment (the same pinning discipline the JIT/clock tripwires use).
func TestProvisionScriptsPinRunMarker(t *testing.T) {
	for _, s := range []struct{ name, script string }{
		{"darwin", provisionScriptDarwin},
		{"linux", provisionScriptLinux},
	} {
		if !strings.Contains(s.script, "exec ./run.sh") {
			t.Errorf("%s provision script missing the exec ./run.sh anchor for guest_env injection", s.name)
		}
	}
}

// rotateServer is an in-process SSH "guest" that behaves like a real one
// under rotation: password auth works until the rotate script lands, the
// capture exec serves its real host key, and the install exec mutates the
// authorized-key set the PublicKeyCallback consults — so Rotate's whole
// mint → capture → install → redial choreography runs over real SSH wire
// behavior, no VM.
type rotateServer struct {
	addr    string
	hostPub ssh.PublicKey

	mu                sync.Mutex
	authorized        map[string]bool
	passwordDisabled  bool
	currentPassword   string // starts "admin"; a landed scramble line updates it
	scripts           []string
	failInstall       bool // the install exec exits 1 (no sudo, say)
	failAuthorize     bool // the install "runs" but the key never authorizes
	forceKeepPassword bool // no drop-in name wins (image quirk); password stays
	failStopRunner    bool // StopRunner exits nonzero (kill unproven)
	failDebugInstall  bool // InstallAuthorizedKey's read-back grep fails
	stopCalls         int
	debugInstalls     int
	lastInstallScript string
	failPush          bool // the push exec exits 1 (e.g. mkdir denied)
	lastPushCmd       string
	lastPushBody      []byte // stdin the push exec received
}

func (s *rotateServer) lastScript(t *testing.T) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.scripts) == 0 {
		t.Fatal("no rotate script reached the guest")
	}
	return s.scripts[len(s.scripts)-1]
}

func (s *rotateServer) lastInstall(t *testing.T) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastInstallScript == "" {
		t.Fatal("no install script reached the guest")
	}
	return s.lastInstallScript
}

func newRotateServer(t *testing.T) *rotateServer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostKey, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	srv := &rotateServer{hostPub: hostKey.PublicKey(), authorized: map[string]bool{}, currentPassword: "admin"}

	conf := &ssh.ServerConfig{
		PasswordCallback: func(meta ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			srv.mu.Lock()
			disabled, want := srv.passwordDisabled, srv.currentPassword
			srv.mu.Unlock()
			if disabled {
				return nil, errors.New("password auth disabled")
			}
			if meta.User() == "admin" && string(pass) == want {
				return nil, nil
			}
			return nil, errors.New("denied")
		},
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			srv.mu.Lock()
			ok := srv.authorized[string(key.Marshal())]
			srv.mu.Unlock()
			if ok {
				return nil, nil
			}
			return nil, errors.New("denied")
		},
	}
	conf.AddHostKey(hostKey)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	srv.addr = ln.Addr().String()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				sc, chans, reqs, err := ssh.NewServerConn(conn, conf)
				if err != nil {
					return
				}
				defer sc.Close()
				go ssh.DiscardRequests(reqs)
				for newCh := range chans {
					if newCh.ChannelType() != "session" {
						_ = newCh.Reject(ssh.UnknownChannelType, "")
						continue
					}
					ch, chReqs, err := newCh.Accept()
					if err != nil {
						continue
					}
					go srv.handleSession(ch, chReqs)
				}
			}()
		}
	}()
	return srv
}

func (s *rotateServer) handleSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	for req := range reqs {
		if req.Type != "exec" {
			_ = req.Reply(false, nil)
			continue
		}
		var payload struct{ Cmd string }
		_ = ssh.Unmarshal(req.Payload, &payload)
		_ = req.Reply(true, nil)

		exit := func(code uint32) {
			b := ssh.Marshal(struct{ C uint32 }{code})
			_, _ = ch.SendRequest("exit-status", false, b)
		}
		switch {
		case strings.Contains(payload.Cmd, "jitconfig"):
			// StopRunner: report the listener already dead (pgrep finds
			// nothing → the while loop never runs → exit 0), unless the test
			// forces a survival.
			s.mu.Lock()
			killUnproven := s.failStopRunner
			s.stopCalls++
			s.mu.Unlock()
			if killUnproven {
				fmt.Fprintln(ch.Stderr(), "runny: runner still alive after SIGKILL")
				exit(1)
			} else {
				exit(0)
			}
		case strings.Contains(payload.Cmd, runnerPushCacheDir):
			// PushRunnerTarball: read back whatever the exec's stdin carries,
			// the same way a real `cat > path` would consume it.
			body, _ := io.ReadAll(ch)
			s.mu.Lock()
			s.lastPushCmd = payload.Cmd
			s.lastPushBody = body
			fail := s.failPush
			s.mu.Unlock()
			if fail {
				fmt.Fprintln(ch.Stderr(), "mkdir: permission denied")
				exit(1)
			} else {
				exit(0)
			}
		case strings.Contains(payload.Cmd, "ssh_host_"):
			_, _ = ch.Write(ssh.MarshalAuthorizedKey(s.hostPub))
			exit(0)
		case strings.Contains(payload.Cmd, "grep -qF"):
			// InstallAuthorizedKey: the printf-append + grep read-back form
			// (the read-back grep is its distinguishing marker).
			s.mu.Lock()
			s.lastInstallScript = payload.Cmd
			s.debugInstalls++
			fail := s.failDebugInstall
			s.mu.Unlock()
			if fail {
				fmt.Fprintln(ch.Stderr(), "grep: no match")
				exit(1)
			} else {
				exit(0)
			}
		case strings.Contains(payload.Cmd, "authorized_keys"):
			s.mu.Lock()
			s.scripts = append(s.scripts, payload.Cmd)
			fail := s.failAuthorize
			if s.failInstall {
				s.mu.Unlock()
				fmt.Fprintln(ch.Stderr(), "sudo: command not found")
				exit(1)
				return
			}
			s.mu.Unlock()
			// "Run" the script: extract the echoed pubkey and authorize it,
			// then apply the drop-in the way sshd actually would — first
			// obtained value wins across lexically-ordered includes, and
			// this "image" ships 50-cloud-init.conf with
			// "PasswordAuthentication yes" (as ubuntu cloud images do). A
			// drop-in that does not sort before it changes nothing, and the
			// rotation must detect that loudly.
			key, err := pubkeyFromScript(payload.Cmd)
			if err != nil {
				fmt.Fprintln(ch.Stderr(), err.Error())
				exit(1)
				return
			}
			s.mu.Lock()
			if !fail {
				s.authorized[string(key.Marshal())] = true
			}
			if name := dropInFromScript(payload.Cmd); name != "" && name < "50-cloud-init.conf" && !s.forceKeepPassword {
				s.passwordDisabled = true
			}
			// A landed scramble line changes the account's real password —
			// same as a real chpasswd/dscl would — independent of whether
			// passwordDisabled flipped above (forceKeepPassword can leave
			// password auth nominally on while the credential underneath it
			// still moves).
			if pw, ok := scramblePasswordFromScript(payload.Cmd); ok {
				s.currentPassword = pw
			}
			s.mu.Unlock()
			exit(0)
		default:
			fmt.Fprintln(ch, "ok")
			exit(0)
		}
		return
	}
}

// dropInFromScript pulls the sshd_config.d filename the script writes —
// the fake's stand-in for sshd's lexical include ordering.
func dropInFromScript(script string) string {
	const marker = "sshd_config.d/"
	i := strings.Index(script, marker)
	if i < 0 {
		return ""
	}
	rest := script[i+len(marker):]
	if j := strings.IndexAny(rest, " \n\t"); j >= 0 {
		return rest[:j]
	}
	return rest
}

// pubkeyFromScript pulls the single-quoted authorized_keys line out of the
// rotate script, the way a shell would hand it to echo.
func pubkeyFromScript(script string) (ssh.PublicKey, error) {
	const marker = "echo '"
	i := strings.Index(script, marker)
	if i < 0 {
		return nil, errors.New("no echoed key in script")
	}
	rest := script[i+len(marker):]
	j := strings.Index(rest, "'")
	if j < 0 {
		return nil, errors.New("unterminated key quote in script")
	}
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(rest[:j]))
	return key, err
}

func testCtx(t *testing.T) bounded.Context {
	t.Helper()
	ctx, cancel := bounded.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func testDialer() Dialer {
	return Dialer{
		SSH:           sshx.Config{User: "admin", Password: "admin", Timeout: 2 * time.Second},
		RetryInterval: 50 * time.Millisecond,
	}
}

// The full rotation choreography: password session in, keyed-and-pinned
// session out, password auth dead behind it.
func TestRotateChoreography(t *testing.T) {
	srv := newRotateServer(t)
	d := testDialer()

	g, err := d.WaitFor(testCtx(t), srv.addr, "darwin")
	if err != nil {
		t.Fatalf("WaitFor: %v", err)
	}
	rg, err := d.Rotate(testCtx(t), srv.addr, g, "linux")
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	// The rotated session execs (it authenticated by the cycle key the
	// install script delivered, against the pinned host key).
	out, code, err := rg.(*Guest).c.Output(testCtx(t), "true")
	if err != nil || code != 0 {
		t.Fatalf("exec over rotated session: %q, %d, %v", out, code, err)
	}

	// The password session was closed by Rotate (its one job is done).
	if _, _, err := g.(*Guest).c.Output(testCtx(t), "true"); err == nil {
		t.Error("password session still usable after successful rotation")
	}

	// And the guest no longer takes the password at all — the mid-cycle
	// `ssh admin@guest` an attacker (or operator) would try.
	if _, err := sshx.Dial(testCtx(t), srv.addr, d.SSH); err == nil {
		t.Error("guest accepted password auth after rotation")
	}

	// The script that reached the guest is the linux one (full content facts
	// live in TestRotateScriptsPerOS, once).
	if script := srv.lastScript(t); !strings.Contains(script, "systemctl reload") {
		t.Errorf("linux rotate script did not reach the guest: %q", script)
	}
}

// Install failure must leave the password session OPEN — teardown pulls the
// post-mortem over it — and name the failing step.
func TestRotateInstallFailureKeepsPasswordSession(t *testing.T) {
	srv := newRotateServer(t)
	srv.mu.Lock()
	srv.failInstall = true
	srv.mu.Unlock()
	d := testDialer()

	g, err := d.WaitFor(testCtx(t), srv.addr, "darwin")
	if err != nil {
		t.Fatalf("WaitFor: %v", err)
	}
	_, err = d.Rotate(testCtx(t), srv.addr, g, "linux")
	if err == nil {
		t.Fatal("Rotate succeeded against a failing install")
	}
	if !strings.Contains(err.Error(), "installing cycle key") || !strings.Contains(err.Error(), "sudo: command not found") {
		t.Errorf("err = %v, want the step and the guest's stderr", err)
	}
	// Post-mortem still possible.
	if _, _, err := g.(*Guest).c.Output(testCtx(t), "true"); err != nil {
		t.Errorf("password session unusable after install failure: %v", err)
	}
}

// Rotate must refuse a foreign Guest implementation rather than guess.
func TestRotateRejectsForeignGuest(t *testing.T) {
	d := testDialer()
	if _, err := d.Rotate(testCtx(t), "127.0.0.1:1", nil, "linux"); err == nil {
		t.Fatal("Rotate accepted a non-guest value")
	}
}

// Per-OS script shape: darwin must NOT reload (launchd socket-activates sshd
// per connection; the config is read at spawn), linux must reload (one
// long-lived sshd) — and reload, never restart.
func TestRotateScriptsPerOS(t *testing.T) {
	if strings.Contains(rotateScriptDarwin, "systemctl") {
		t.Error("darwin rotate script must not reference systemctl")
	}
	if !strings.Contains(rotateScriptLinux, "systemctl reload") {
		t.Error("linux rotate script must reload sshd")
	}
	if strings.Contains(rotateScriptLinux, "systemctl restart") {
		t.Error("reload, NOT restart: restart would kill the established session")
	}
	for _, script := range []string{rotateScriptDarwin, rotateScriptLinux} {
		if !strings.Contains(script, "set -e") || !strings.Contains(script, "umask 077") {
			t.Error("rotate scripts must fail fast and create key material 0600")
		}
		for _, want := range []string{"PasswordAuthentication no", "KbdInteractiveAuthentication no"} {
			if !strings.Contains(script, want) {
				t.Errorf("rotate script missing %q", want)
			}
		}
	}
	// sshd takes the FIRST obtained value per keyword across lexically
	// ordered includes; image fleets ship their own auth drop-ins (ubuntu
	// cloud images: 50-cloud-init.conf "PasswordAuthentication yes"). The
	// drop-in must sort before them or it is a dead no-op.
	name := dropInFromScript(rotateScriptBase)
	if name == "" || name >= "50-cloud-init.conf" {
		t.Errorf("drop-in %q does not sort before 50-cloud-init.conf; sshd would ignore it", name)
	}
}

// ssh_hardening: scramble is opt-in — plain rotate must never touch the
// account password.
func TestRotatePlainDoesNotScramblePassword(t *testing.T) {
	srv := newRotateServer(t)
	d := testDialer()

	g, err := d.WaitFor(testCtx(t), srv.addr, "linux")
	if err != nil {
		t.Fatalf("WaitFor: %v", err)
	}
	if _, err := d.Rotate(testCtx(t), srv.addr, g, "linux"); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if script := srv.lastScript(t); strings.Contains(script, "chpasswd") || strings.Contains(script, "dscl") {
		t.Errorf("plain rotate must not scramble the password: %q", script)
	}
}

// ssh_hardening: scramble appends the password-scramble line to the same
// rotate script (one exec, one set -e), and mints a fresh, different
// password every cycle — a static or reused value would defeat the point.
func TestRotateScrambleAppendsPasswordChange(t *testing.T) {
	srv := newRotateServer(t)
	d := testDialer()
	d.Hardening = home.SSHHardeningScramble

	g, err := d.WaitFor(testCtx(t), srv.addr, "linux")
	if err != nil {
		t.Fatalf("WaitFor: %v", err)
	}
	if _, err := d.Rotate(testCtx(t), srv.addr, g, "linux"); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	script := srv.lastScript(t)
	if !strings.Contains(script, "| sudo chpasswd") {
		t.Errorf("linux scramble script missing chpasswd: %q", script)
	}
	pw1, ok := scramblePasswordFromScript(script)
	if !ok {
		t.Fatal("no scrambled password in script")
	}

	srv2 := newRotateServer(t)
	d2 := testDialer()
	d2.Hardening = home.SSHHardeningScramble
	g2, err := d2.WaitFor(testCtx(t), srv2.addr, "linux")
	if err != nil {
		t.Fatalf("WaitFor: %v", err)
	}
	if _, err := d2.Rotate(testCtx(t), srv2.addr, g2, "linux"); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	pw2, ok := scramblePasswordFromScript(srv2.lastScript(t))
	if !ok {
		t.Fatal("no scrambled password in second script")
	}
	if pw1 == pw2 {
		t.Error("scrambled password reused across cycles")
	}
}

// darwin's dscl has no stdin form for -passwd, so the scramble line must use
// dscl directly rather than the linux chpasswd form.
func TestRotateScrambleUsesDsclOnDarwin(t *testing.T) {
	srv := newRotateServer(t)
	d := testDialer()
	d.Hardening = home.SSHHardeningScramble

	g, err := d.WaitFor(testCtx(t), srv.addr, "darwin")
	if err != nil {
		t.Fatalf("WaitFor: %v", err)
	}
	if _, err := d.Rotate(testCtx(t), srv.addr, g, "darwin"); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	script := srv.lastScript(t)
	if !strings.Contains(script, "dscl . -passwd") {
		t.Errorf("darwin scramble script missing dscl: %q", script)
	}
	if strings.Contains(script, "chpasswd") {
		t.Error("darwin scramble script must not use linux's chpasswd form")
	}
}

// A guest where the drop-in silently loses PLUS scramble is on must still be
// a LOUD rotation failure. verifyPasswordAuthDead has to dial with the
// freshly scrambled password, not the stale pool default — d.SSH.Password
// stops being the guest's real password the instant the scramble line
// lands, so proving the OLD value is rejected would prove nothing about
// whether password auth is actually dead.
func TestRotateScrambleFailsLoudWhenPasswordSurvives(t *testing.T) {
	srv := newRotateServer(t)
	srv.mu.Lock()
	srv.forceKeepPassword = true
	srv.mu.Unlock()
	d := testDialer()
	d.Hardening = home.SSHHardeningScramble

	g, err := d.WaitFor(testCtx(t), srv.addr, "darwin")
	if err != nil {
		t.Fatalf("WaitFor: %v", err)
	}
	ctx, cancel := bounded.WithTimeout(t.Context(), 1500*time.Millisecond)
	defer cancel()
	_, err = d.Rotate(ctx, srv.addr, g, "linux")
	if err == nil {
		t.Fatal("Rotate reported success while the guest still accepts password auth")
	}
	if !strings.Contains(err.Error(), "password auth still alive") {
		t.Errorf("err = %v, want the un-hardened diagnosis", err)
	}
}

// scramblePasswordFromScript pulls the single-quoted password out of either
// the linux (chpasswd) or darwin (dscl) scramble line, the same way
// pubkeyFromScript reads the echoed key. ok is false when the script carries
// no scramble line at all (plain rotate never gets one).
func scramblePasswordFromScript(script string) (pw string, ok bool) {
	for _, marker := range []string{`"$(id -un)" '`, `"/Users/$(id -un)" '`} {
		i := strings.Index(script, marker)
		if i < 0 {
			continue
		}
		rest := script[i+len(marker):]
		if j := strings.Index(rest, "'"); j >= 0 {
			return rest[:j], true
		}
	}
	return "", false
}

// A guest where the drop-in silently loses (sshd config precedence, a
// missing include, an image quirk) must be a LOUD rotation failure: the
// keyed redial succeeding is not enough — password auth being dead is the
// point, and Rotate must prove it.
func TestRotateFailsLoudWhenPasswordSurvives(t *testing.T) {
	srv := newRotateServer(t)
	srv.mu.Lock()
	srv.forceKeepPassword = true
	srv.mu.Unlock()
	d := testDialer()

	g, err := d.WaitFor(testCtx(t), srv.addr, "darwin")
	if err != nil {
		t.Fatalf("WaitFor: %v", err)
	}
	// Short budget: the verification loop polls until ctx expiry once it
	// keeps finding the password alive.
	ctx, cancel := bounded.WithTimeout(t.Context(), 1500*time.Millisecond)
	defer cancel()
	_, err = d.Rotate(ctx, srv.addr, g, "linux")
	if err == nil {
		t.Fatal("Rotate reported success while the guest still accepts the password")
	}
	if !strings.Contains(err.Error(), "password auth still alive") {
		t.Errorf("err = %v, want the un-hardened diagnosis", err)
	}
	// The password session is still the cycle's guest; post-mortem rides it.
	if _, _, err := g.(*Guest).c.Output(testCtx(t), "true"); err != nil {
		t.Errorf("password session unusable after verification failure: %v", err)
	}
}

// Redial failure (key never authorizes — non-default AuthorizedKeysFile,
// StrictModes rejection) must leave the password session open for teardown's
// post-mortem and name the step.
func TestRotateRedialFailureKeepsPasswordSession(t *testing.T) {
	srv := newRotateServer(t)
	srv.mu.Lock()
	srv.failAuthorize = true
	srv.mu.Unlock()
	d := testDialer()

	g, err := d.WaitFor(testCtx(t), srv.addr, "darwin")
	if err != nil {
		t.Fatalf("WaitFor: %v", err)
	}
	ctx, cancel := bounded.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, err = d.Rotate(ctx, srv.addr, g, "linux")
	if err == nil {
		t.Fatal("Rotate succeeded with the key never authorized")
	}
	if !strings.Contains(err.Error(), "reconnecting with cycle key") {
		t.Errorf("err = %v, want the redial step named", err)
	}
	if _, _, err := g.(*Guest).c.Output(testCtx(t), "true"); err != nil {
		t.Errorf("password session unusable after redial failure: %v", err)
	}
}

// Host-key parsing fails loudly: proceeding unpinned would silently undo the
// defense, and a skipped unparseable key could exclude the very key the
// redial negotiates.
func TestParseHostKeysLoudFailures(t *testing.T) {
	if _, err := parseHostKeys([]byte("")); err == nil {
		t.Error("empty capture must fail")
	}
	if _, err := parseHostKeys([]byte("cat: /etc/ssh/ssh_host_rsa_key.pub: Permission denied\n")); err == nil {
		t.Error("garbage line must fail, not be skipped")
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	line := ssh.MarshalAuthorizedKey(signer.PublicKey())
	keys, err := parseHostKeys(append(line, line...))
	if err != nil || len(keys) != 2 {
		t.Errorf("parseHostKeys(2 lines) = %d keys, %v", len(keys), err)
	}
}

// StopRunner proves the listener dead: a clean exit succeeds, a nonzero exit
// (kill unproven) is a loud error (issue #39).
func TestStopRunner(t *testing.T) {
	srv := newRotateServer(t)
	d := testDialer()
	g, err := d.WaitFor(testCtx(t), srv.addr, "darwin")
	if err != nil {
		t.Fatalf("WaitFor: %v", err)
	}
	if err := g.(*Guest).StopRunner(testCtx(t)); err != nil {
		t.Fatalf("StopRunner (proven dead): %v", err)
	}
	srv.mu.Lock()
	srv.failStopRunner = true
	srv.mu.Unlock()
	if err := g.(*Guest).StopRunner(testCtx(t)); err == nil {
		t.Fatal("StopRunner must fail when the kill is unproven")
	}
}

// The stop script's [-]-jitconfig pattern matches the listener argv without
// matching its own literal text.
func TestStopRunnerPatternIsSelfExcluding(t *testing.T) {
	if !strings.Contains(stopRunnerScript, `[-]-jitconfig`) {
		t.Error("stop script must use the self-excluding bracket pattern")
	}
	if strings.Contains(stopRunnerScript, `'--jitconfig'`) {
		t.Error("stop script must not contain the literal --jitconfig (it would self-match)")
	}
}

// PushRunnerTarball streams the local file's exact bytes to the guest over
// the same `cat >`-style exec pattern StartRunner already uses for the JIT
// config, landing at $HOME/runny-cache/<basename>.
func TestPushRunnerTarball(t *testing.T) {
	srv := newRotateServer(t)
	d := testDialer()
	g, err := d.WaitFor(testCtx(t), srv.addr, "linux")
	if err != nil {
		t.Fatalf("WaitFor: %v", err)
	}

	const want = "not a real tarball, just spike bytes"
	local := filepath.Join(t.TempDir(), "actions-runner-linux-amd64-2.320.0.tar.gz")
	if err := os.WriteFile(local, []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := g.(*Guest).PushRunnerTarball(testCtx(t), local); err != nil {
		t.Fatalf("PushRunnerTarball: %v", err)
	}

	srv.mu.Lock()
	cmd, body := srv.lastPushCmd, string(srv.lastPushBody)
	srv.mu.Unlock()
	if body != want {
		t.Errorf("guest received body %q, want %q", body, want)
	}
	if !strings.Contains(cmd, `$HOME/`+runnerPushCacheDir+`/actions-runner-linux-amd64-2.320.0.tar.gz`) {
		t.Errorf("push command %q must target $HOME/%s/<basename>", cmd, runnerPushCacheDir)
	}
	if !strings.Contains(cmd, "mkdir -p") {
		t.Errorf("push command %q must create the cache dir first", cmd)
	}
}

// A failed push (mkdir denied, say) is a loud error, not swallowed.
func TestPushRunnerTarballFailureSurfaces(t *testing.T) {
	srv := newRotateServer(t)
	srv.failPush = true
	d := testDialer()
	g, err := d.WaitFor(testCtx(t), srv.addr, "linux")
	if err != nil {
		t.Fatalf("WaitFor: %v", err)
	}

	local := filepath.Join(t.TempDir(), "actions-runner-linux-amd64-2.320.0.tar.gz")
	if err := os.WriteFile(local, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := g.(*Guest).PushRunnerTarball(testCtx(t), local); err == nil {
		t.Fatal("PushRunnerTarball must fail when the guest exec exits nonzero")
	}
}

// PushRunnerTarball refuses a tarball basename that doesn't match the same
// trust-boundary charset provisionScript enforces — it crosses into a shell
// command string here too.
func TestPushRunnerTarballRejectsBadName(t *testing.T) {
	srv := newRotateServer(t)
	d := testDialer()
	g, err := d.WaitFor(testCtx(t), srv.addr, "linux")
	if err != nil {
		t.Fatalf("WaitFor: %v", err)
	}
	local := filepath.Join(t.TempDir(), "actions-runner$(whoami).tar.gz")
	if err := os.WriteFile(local, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := g.(*Guest).PushRunnerTarball(testCtx(t), local); err == nil {
		t.Fatal("PushRunnerTarball must refuse a tarball name with shell metacharacters")
	}
}

// InstallAuthorizedKey appends and read-back-greps; a failed read-back is a
// loud error.
func TestInstallAuthorizedKey(t *testing.T) {
	srv := newRotateServer(t)
	d := testDialer()
	g, err := d.WaitFor(testCtx(t), srv.addr, "darwin")
	if err != nil {
		t.Fatalf("WaitFor: %v", err)
	}
	if err := g.(*Guest).InstallAuthorizedKey(testCtx(t), "ssh-ed25519 AAAAOPKEY op@host"); err != nil {
		t.Fatalf("InstallAuthorizedKey: %v", err)
	}
	srv.mu.Lock()
	srv.failDebugInstall = true
	srv.mu.Unlock()
	if err := g.(*Guest).InstallAuthorizedKey(testCtx(t), "ssh-ed25519 AAAAOPKEY op@host"); err == nil {
		t.Fatal("InstallAuthorizedKey must fail when the read-back fails")
	}
}

// HostKeys renders pinned host keys in known_hosts form, host from the addr.
func TestHostKeysKnownHostsForm(t *testing.T) {
	srv := newRotateServer(t)
	d := testDialer()
	g, err := d.WaitFor(testCtx(t), srv.addr, "darwin")
	if err != nil {
		t.Fatalf("WaitFor: %v", err)
	}
	// An unhardened guest (no pins) returns nil.
	if hk := g.(*Guest).HostKeys(); hk != nil {
		t.Errorf("unpinned HostKeys = %v, want nil", hk)
	}
	// After rotation, the pinned host key renders as "<host> <type> <b64>".
	rg, err := d.Rotate(testCtx(t), srv.addr, g, "linux")
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	host, _, _ := net.SplitHostPort(srv.addr)
	hk := rg.(*Guest).HostKeys()
	if len(hk) != 1 {
		t.Fatalf("HostKeys = %v, want 1 pinned key", hk)
	}
	if !strings.HasPrefix(hk[0], host+" ssh-ed25519 ") {
		t.Errorf("HostKeys[0] = %q, want %q-prefixed known_hosts line", hk[0], host)
	}
}

// The authorized_keys line installed by InstallAuthorizedKey must carry a
// command= recording wrapper and restrict,pty options; the daemon's own cycle
// key (installed by rotateScriptBase) must remain unwrapped.
func TestInstallDebugKeyLineHasCommandWrapper(t *testing.T) {
	srv := newRotateServer(t)
	d := testDialer()
	g, err := d.WaitFor(testCtx(t), srv.addr, "darwin")
	if err != nil {
		t.Fatalf("WaitFor: %v", err)
	}
	if err := g.(*Guest).InstallAuthorizedKey(testCtx(t), "ssh-ed25519 AAAAOPKEY op@host"); err != nil {
		t.Fatalf("InstallAuthorizedKey: %v", err)
	}
	script := srv.lastInstall(t)
	const prefix = `command="exec /tmp/runny-record",restrict,pty`
	if !strings.Contains(script, prefix) {
		t.Errorf("installed line missing command= wrapper\nscript: %q", script)
	}
	// The daemon's own key (rotateScriptBase) must NOT carry command=.
	if strings.Contains(rotateScriptBase, "command=") {
		t.Error("daemon cycle key must not be wrapped with command=")
	}
}

// The per-OS debug recorder uses the correct script(1) calling convention:
// BSD (darwin) uses the positional form, util-linux uses -c.
func TestInstallDebugKeyRecorderPerOS(t *testing.T) {
	// Darwin: BSD positional form for non-interactive execution.
	if !strings.Contains(debugRecorderDarwin, `/bin/sh -c "$SSH_ORIGINAL_COMMAND"`) {
		t.Error("darwin recorder must use BSD positional form: script FILE /bin/sh -c CMD")
	}
	if strings.Contains(debugRecorderDarwin, `-c "$SSH_ORIGINAL_COMMAND" -e`) {
		t.Error("darwin recorder must not use util-linux -c flag form")
	}
	// Linux: util-linux flag form.
	if !strings.Contains(debugRecorderLinux, `-c "$SSH_ORIGINAL_COMMAND"`) {
		t.Error("linux recorder must use util-linux -c flag form")
	}
	if !strings.Contains(debugRecorderLinux, `-e`) {
		t.Error("linux recorder must carry -e to propagate child exit code")
	}
	if strings.Contains(debugRecorderLinux, `/bin/sh -c "$SSH_ORIGINAL_COMMAND"`) {
		t.Error("linux recorder must not use BSD positional form")
	}
	// Flush flags: -F on Darwin (BSD script), -f on Linux (util-linux) — different
	// flag letters, same "flush after each write" semantics.
	if !strings.Contains(debugRecorderDarwin, " -F ") {
		t.Error("darwin recorder must use -F (flush) for in-progress session reads")
	}
	if !strings.Contains(debugRecorderLinux, " -f ") {
		t.Error("linux recorder must use -f (flush) for in-progress session reads")
	}
	// Both: fallback when script is absent must respect SSH_ORIGINAL_COMMAND.
	for name, r := range map[string]string{"darwin": debugRecorderDarwin, "linux": debugRecorderLinux} {
		if !strings.Contains(r, "SSH_ORIGINAL_COMMAND") {
			t.Errorf("%s recorder fallback must handle SSH_ORIGINAL_COMMAND", name)
		}
	}
	// Both: append mode and quiet mode on every exec.
	for name, r := range map[string]string{"darwin": debugRecorderDarwin, "linux": debugRecorderLinux} {
		if !strings.Contains(r, " -q ") {
			t.Errorf("%s recorder must use -q (quiet)", name)
		}
		if !strings.Contains(r, " -a") {
			t.Errorf("%s recorder must use -a (append)", name)
		}
		// Fallback when script is absent.
		if !strings.Contains(r, "command -v script") {
			t.Errorf("%s recorder must check for script availability", name)
		}
		if !strings.Contains(r, "SHELL") {
			t.Errorf("%s recorder must fall back to $SHELL when script is absent", name)
		}
	}
}

// The linux recorder (util-linux script -c form) is selected when WaitFor
// receives goos="linux", even on the unhardened path where Rotate is never
// called.
func TestInstallDebugKeyUsesLinuxRecorderOnLinuxGuest(t *testing.T) {
	srv := newRotateServer(t)
	d := testDialer()
	g, err := d.WaitFor(testCtx(t), srv.addr, "linux")
	if err != nil {
		t.Fatalf("WaitFor: %v", err)
	}
	if err := g.(*Guest).InstallAuthorizedKey(testCtx(t), "ssh-ed25519 AAAAOPKEY op@host"); err != nil {
		t.Fatalf("InstallAuthorizedKey: %v", err)
	}
	script := srv.lastInstall(t)
	if strings.Contains(script, `/bin/sh -c "$SSH_ORIGINAL_COMMAND"`) {
		t.Errorf("linux guest got Darwin (BSD positional) recorder\nscript: %q", script)
	}
	if !strings.Contains(script, `-c "$SSH_ORIGINAL_COMMAND" -e`) {
		t.Errorf("linux guest missing util-linux -c flag form\nscript: %q", script)
	}
}

// The windows rotate script must set the administrators_authorized_keys ACL
// (sshd silently ignores the file otherwise), PREPEND
// PasswordAuthentication no (sshd_config is first-match-wins, and the stock
// config ends with a Match Group administrators block an append would land
// inside), and restart sshd via a DETACHED process (an inline restart would
// kill the session issuing it).
func TestRotateScriptWindowsPinsACLPrependAndDetachedRestart(t *testing.T) {
	script := fmt.Sprintf(rotateScriptWindowsTemplate, "AAAA key", "")
	if !strings.Contains(script, `icacls 'C:\ProgramData\ssh\administrators_authorized_keys' /inheritance:r /grant "SYSTEM:F" /grant "BUILTIN\Administrators:F"`) {
		t.Errorf("windows rotate script missing the administrators_authorized_keys ACL fix:\n%s", script)
	}
	if !strings.Contains(script, "if ($LASTEXITCODE -ne 0) { exit 2 }") {
		t.Errorf("windows rotate script must fail loudly when icacls itself errors:\n%s", script)
	}
	// The new directive must be prepended (built as directive + $existing),
	// never appended ($existing + directive).
	if !strings.Contains(script, `"PasswordAuthentication no`) {
		t.Fatalf("windows rotate script missing the PasswordAuthentication directive:\n%s", script)
	}
	if strings.Contains(script, `$existing + "PasswordAuthentication no`) {
		t.Error("windows rotate script appends PasswordAuthentication no instead of prepending it")
	}
	if !strings.Contains(script, `+ $existing)`) {
		t.Errorf("windows rotate script must prepend the directive before $existing:\n%s", script)
	}
	if !strings.Contains(script, "Start-Process") || !strings.Contains(script, "Start-Sleep -Seconds 1; Restart-Service sshd") {
		t.Errorf("windows rotate script must restart sshd via a detached, delayed process:\n%s", script)
	}
	// The restart must not run inline in this exec — that would kill the
	// session issuing the restart before it could reply.
	if strings.Contains(script, "\nRestart-Service sshd\n") {
		t.Error("windows rotate script must not call Restart-Service inline")
	}
}

// Plain rotate must never touch the account password; scramble uses
// Set-LocalUser (net user prompts interactively above 14 chars and would
// hang the exec).
func TestRotateScrambleWindowsUsesSetLocalUser(t *testing.T) {
	if strings.Contains(fmt.Sprintf(scrambleLineWindowsTemplate, "x"), "net user") {
		t.Error("windows scramble line must not use net user")
	}
	if !strings.Contains(scrambleLineWindowsTemplate, "Set-LocalUser") {
		t.Error("windows scramble line must use Set-LocalUser")
	}
	if !strings.Contains(scrambleLineWindowsTemplate, "-Name Administrator") {
		t.Error("windows scramble line must target the baked Administrator account")
	}
}

// The windows stop script matches the listener by --jitconfig in its
// CommandLine (Get-CimInstance Win32_Process's CommandLine is the windows
// equivalent of pgrep -f) and proves death by re-checking.
func TestStopRunnerScriptWindowsShape(t *testing.T) {
	if !strings.Contains(stopRunnerScriptWindows, "Win32_Process") {
		t.Error("windows stop script must enumerate processes via Win32_Process")
	}
	if !strings.Contains(stopRunnerScriptWindows, "--jitconfig") {
		t.Error("windows stop script must match on --jitconfig")
	}
	if !strings.Contains(stopRunnerScriptWindows, "Stop-Process") {
		t.Error("windows stop script must kill via Stop-Process")
	}
	if !strings.Contains(stopRunnerScriptWindows, "exit 0") || !strings.Contains(stopRunnerScriptWindows, "exit 1") {
		t.Error("windows stop script must have both a proven-dead and a survived exit path")
	}
}

// startRunnerWindows refuses a runner asset name that isn't a well-formed
// zip basename, the same trust-boundary discipline the POSIX path applies.
func TestStartRunnerWindowsRejectsBadZipName(t *testing.T) {
	g := &Guest{goos: home.OSWindows}
	if _, err := g.startRunnerWindows(t.Context(), "jit", "actions-runner-win-x64-2.320.0.tar.gz"); err == nil {
		t.Error("startRunnerWindows accepted a non-.zip runner asset name")
	}
}

// extractRunnerZipScript must make tar's own result the script's exit code
// explicitly — native tar's exit-code propagation through -EncodedCommand is
// version/mode-fragile, and a corrupt zip could otherwise read as success.
func TestExtractRunnerZipScriptPropagatesExitCode(t *testing.T) {
	script := extractRunnerZipScript("actions-runner-win-x64-2.320.0.zip")
	if !strings.HasSuffix(strings.TrimRight(script, "\n"), "exit $LASTEXITCODE") {
		t.Errorf("extract script must end with exit $LASTEXITCODE:\n%s", script)
	}
	if !strings.Contains(script, "tar -xf") {
		t.Errorf("extract script must still invoke tar:\n%s", script)
	}
}

// deliverJITConfigScript copies stdin to the .tmp path and renames it into
// place in ONE script — no separate move exec — with -ErrorAction Stop on
// the Move-Item so a failed rename (Move-Item's own errors are
// non-terminating by default) doesn't silently exit 0.
func TestDeliverJITConfigScriptCommitsAtomically(t *testing.T) {
	script := deliverJITConfigScript()
	if !strings.Contains(script, "[IO.File]::Create('"+jitPendingPathWindows+"')") {
		t.Errorf("script must copy stdin to the .tmp path first:\n%s", script)
	}
	if !strings.Contains(script, "Move-Item -Force -Path '"+jitPendingPathWindows+"' -Destination '"+jitPathWindows+"' -ErrorAction Stop") {
		t.Errorf("script must rename into place with -ErrorAction Stop:\n%s", script)
	}
}

// The rotate and debug-key install scripts must share the exact same
// administrators_authorized_keys ACL fragment — including the icacls
// exit-code check (icacls failing loudly rather than being swallowed by
// | Out-Null, the one condition under which a "successful" key append does
// nothing) — so the ACL fix has exactly one home.
func TestWindowsAuthorizedKeyScriptsShareACLFragment(t *testing.T) {
	fragment := psAppendAuthorizedKeyLine("$x")
	if !strings.Contains(fragment, "if ($LASTEXITCODE -ne 0) { exit 2 }") {
		t.Errorf("shared ACL fragment must fail loudly on a failed icacls: %q", fragment)
	}
	rotate := fmt.Sprintf(rotateScriptWindowsTemplate, "AAAA key", "")
	debug := fmt.Sprintf(installDebugKeyScriptWindows, "AAAA key", "AAAA key")
	for _, s := range []struct {
		name, script string
	}{{"rotate", rotate}, {"debug key", debug}} {
		if !strings.Contains(s.script, "if ($LASTEXITCODE -ne 0) { exit 2 }") {
			t.Errorf("%s script missing the icacls exit-code check:\n%s", s.name, s.script)
		}
	}
}

// The watcher script tails the launcher's log/exit-code contract and exits
// with the runner's own exit code — the anchor points StartRunner's windows
// hand-off relies on. Output goes to the raw stdout stream (never re-encoded
// through [Console]::Out's OEM-codepage TextWriter, which would mangle
// non-ASCII output and can split a multibyte sequence at a drain boundary).
// The exit-code read is guarded by a digits-only regex before the cast.
func TestWatcherScriptWindowsContract(t *testing.T) {
	if !strings.Contains(watcherScriptWindows, runnerLogPathWindows) {
		t.Error("watcher script must tail the launcher's log path")
	}
	if !strings.Contains(watcherScriptWindows, runnerExitPathWindows) {
		t.Error("watcher script must watch for the launcher's exit-code file")
	}
	if !strings.Contains(watcherScriptWindows, "exit ([int]$code)") {
		t.Error("watcher script must exit with the runner's own exit code")
	}
	if !strings.Contains(watcherScriptWindows, `$code -match '^\d+$'`) {
		t.Error("watcher script must guard the exit-code cast with a digits-only regex")
	}
	if strings.Contains(watcherScriptWindows, "[Console]::Out.Write") {
		t.Error("watcher script must not write through [Console]::Out (OEM codepage re-encoding)")
	}
	if !strings.Contains(watcherScriptWindows, "OpenStandardOutput()") {
		t.Error("watcher script must write raw bytes via the standard-output stream")
	}
}

// Redial re-establishes the keyed session against the retained config.
func TestRedialSwapsClient(t *testing.T) {
	srv := newRotateServer(t)
	d := testDialer()
	g, err := d.WaitFor(testCtx(t), srv.addr, "darwin")
	if err != nil {
		t.Fatalf("WaitFor: %v", err)
	}
	rg, err := d.Rotate(testCtx(t), srv.addr, g, "linux")
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if err := rg.(*Guest).Redial(testCtx(t)); err != nil {
		t.Fatalf("Redial: %v", err)
	}
	// The redialed (keyed, pinned) session still execs.
	if err := rg.(*Guest).StopRunner(testCtx(t)); err != nil {
		t.Errorf("exec over redialed session failed: %v", err)
	}
}
