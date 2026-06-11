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
		case strings.Contains(payload.Cmd, "ssh_host_"):
			_, _ = ch.Write(ssh.MarshalAuthorizedKey(s.hostPub))
			exit(0)
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

	g, err := d.WaitFor(testCtx(t), srv.addr)
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

	g, err := d.WaitFor(testCtx(t), srv.addr)
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

	g, err := d.WaitFor(testCtx(t), srv.addr)
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

	g, err := d.WaitFor(testCtx(t), srv.addr)
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
