package guest

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/bojanrajkovic/runny/internal/bounded"
	"github.com/bojanrajkovic/runny/internal/sshx"
)

// The darwin runner launches over a non-login SSH exec, whose PATH lacks
// /usr/local/bin; the provision script must rebuild a login PATH so job steps
// find tools that pkg installers symlink there. Regression guard for the
// "aws: command not found" right after a successful install.
func TestProvisionScriptDarwinPrimesPATH(t *testing.T) {
	if !strings.Contains(provisionScriptDarwin, "/usr/libexec/path_helper") {
		t.Error("darwin provision script must rebuild PATH via path_helper before launching the runner")
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
		script, err := provisionScript(tc.goos, tc.name)
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
		if _, err := provisionScript("darwin", bad); err == nil {
			t.Errorf("provisionScript accepted an unsafe tarball name %q", bad)
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
	scripts           []string
	failInstall       bool // the install exec exits 1 (no sudo, say)
	failAuthorize     bool // the install "runs" but the key never authorizes
	forceKeepPassword bool // no drop-in name wins (image quirk); password stays
	failStopRunner    bool // StopRunner exits nonzero (kill unproven)
	failDebugInstall  bool // InstallAuthorizedKey's read-back grep fails
	stopCalls         int
	debugInstalls     int
	lastInstallScript string
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
	srv := &rotateServer{hostPub: hostKey.PublicKey(), authorized: map[string]bool{}}

	conf := &ssh.ServerConfig{
		PasswordCallback: func(meta ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			srv.mu.Lock()
			disabled := srv.passwordDisabled
			srv.mu.Unlock()
			if disabled {
				return nil, errors.New("password auth disabled")
			}
			if meta.User() == "admin" && string(pass) == "admin" {
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
	// Linux only: -f flushes after each write so in-progress teardown reads
	// get the latest data (util-linux only; BSD script has no -f flag).
	if !strings.Contains(debugRecorderLinux, " -f ") {
		t.Error("linux recorder must use -f (flush) for in-progress session reads")
	}
	if strings.Contains(debugRecorderDarwin, " -f ") {
		t.Error("darwin recorder must not use -f (BSD script has no flush flag)")
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
