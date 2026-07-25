// Package guest adapts sshx sessions to the state machine's Guest interface:
// it knows how a cirruslabs guest stages the actions runner from the virtiofs
// cache share and launches run.sh with a JIT config.
//
// This file holds the shared machinery: the Guest/Dialer/proc types, perOS,
// WaitFor, and the dispatcher methods (Rotate, StartRunner, PushRunnerTarball,
// StopRunner, PullDiag, PullDebugSession, InstallAuthorizedKey) that contain
// both the POSIX and windows dialects inline. The dialect-specific scripts and
// helpers live in guest_posix.go (darwin/linux) and winguest.go (windows) —
// see internal/guest/CLAUDE.md for the split's rationale.
package guest

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/bojanrajkovic/runny/internal/bounded"
	"github.com/bojanrajkovic/runny/internal/home"
	"github.com/bojanrajkovic/runny/internal/sshx"
	"github.com/bojanrajkovic/runny/internal/statemachine"
)

// Dialer implements statemachine.Dialer over sshx.
type Dialer struct {
	SSH sshx.Config
	// RetryInterval between connection attempts (default 2s).
	RetryInterval time.Duration
	// Hardening is the pool's home.SSHHardeningMode. Rotate consults
	// Hardening.Scrambles() to decide whether to also randomize the guest
	// account password; the FSM's own gate (ssh_hardening != off) already
	// decides whether Rotate is called at all.
	Hardening home.SSHHardeningMode
}

func (d Dialer) interval() time.Duration {
	if d.RetryInterval != 0 {
		return d.RetryInterval
	}
	return 2 * time.Second
}

func (d Dialer) WaitFor(ctx bounded.Context, addr, goos string) (statemachine.Guest, error) {
	c, err := sshx.WaitFor(ctx, addr, d.SSH, d.interval())
	if err != nil {
		return nil, err
	}
	return &Guest{c: c, addr: addr, cfg: d.SSH, interval: d.interval(), goos: goos}, nil
}

// Guest is one authenticated session into a booted runner VM. It retains the
// addr and sshx.Config that built its current client so it can Redial after a
// transport death (a guest reboot mid-DEBUG, issue #39) and so HostKeys can
// surface the pins.
type Guest struct {
	c        *sshx.Client
	addr     string
	cfg      sshx.Config
	interval time.Duration
	// goos is the guest OS; set by Rotate and WaitFor.
	// InstallAuthorizedKey uses it to select the per-OS session recorder.
	goos string
}

// perOS selects darwinVal or linuxVal for goos, erroring on anything else —
// including "windows": every windows call site dispatches to its own
// windows-specific path before perOS ever runs (StartRunner, Rotate,
// PushRunnerTarball, StopRunner, PullDiag, InstallAuthorizedKey, PullDebugSession
// all branch on goos == home.OSWindows first), so perOS itself only ever
// needs to pick between the two POSIX scripts. An unrecognized goos reaching
// it — windows included — is a loud error, never a silent darwin fallback:
// home.validate() already gates every config-declared pool os, so this only
// ever fires against a value this package computed itself, a bug to fix, not
// a case to swallow.
func perOS(goos, darwinVal, linuxVal string) (string, error) {
	switch goos {
	case home.OSDarwin:
		return darwinVal, nil
	case home.OSLinux:
		return linuxVal, nil
	default:
		return "", fmt.Errorf("guest: unknown guest os %q", goos)
	}
}

// Rotate hardens an authenticated session: mint an in-memory
// per-cycle ed25519 key, capture the guest's host keys, install the key and
// disable password auth over the existing password session, reconnect
// authenticated by the key with the host keys pinned — then PROVE the
// password is dead by attempting it and requiring rejection. The private key
// never touches disk and dies with this process.
//
// When d.Hardening is "scramble", the same pre-flip exec also randomizes the
// guest account's password (issue #210), so the image's well-known default
// is never reachable again for the rest of the cycle through any channel,
// not just SSH password auth. verifyPasswordAuthDead below is re-pointed at
// that fresh password in this case — it must prove the guest's CURRENT
// credential is rejected, not the stale pool default: once the scramble
// line lands, the pool default simply stops being the guest's password, so
// dialing with it would fail regardless of whether PasswordAuthentication
// actually flipped, silently defeating the "prove the negative" check. There
// is no way to prove the scramble itself took effect — guest control is SSH
// only, and PasswordAuthentication no already blocks password auth
// regardless of the account password's value, so the script's exit code is
// the only signal, same trust level the rest of this script already gets.
//
// The password session is closed only when all of that succeeded. On every
// failure path it is deliberately left open: the FSM still owns g, the
// established session survives the flip on both OSes (linux reloads, macOS
// spawns sshd per connection), and teardown both pulls the post-mortem diag
// over it and closes it.
func (d Dialer) Rotate(ctx bounded.Context, addr string, g statemachine.Guest, goos string) (statemachine.Guest, error) {
	// Demanding the concrete type is safe: this Dialer created g in WaitFor
	// and the FSM hands the same value back. The FSM's test fakes implement
	// the statemachine.Dialer seam and never reach this code.
	pg, ok := g.(*Guest)
	if !ok {
		return nil, fmt.Errorf("rotate: guest is %T, not a guest session", g)
	}

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("rotate: minting cycle key: %w", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, fmt.Errorf("rotate: cycle key signer: %w", err)
	}

	if goos == home.OSWindows {
		return d.rotateWindows(ctx, addr, pg, signer)
	}

	out, code, err := pg.c.Output(ctx, captureHostKeys)
	if err != nil {
		return nil, fmt.Errorf("rotate: capturing host keys: %w", err)
	}
	if code != 0 {
		return nil, fmt.Errorf("rotate: capturing host keys: exit %d: %s", code, out)
	}
	hostKeys, err := parseHostKeys(out)
	if err != nil {
		return nil, fmt.Errorf("rotate: %w", err)
	}

	script, err := perOS(goos, rotateScriptDarwin, rotateScriptLinux)
	if err != nil {
		return nil, fmt.Errorf("rotate: %w", err)
	}
	pubLine := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	full := fmt.Sprintf(script, pubLine)

	// verifyPW is the password verifyPasswordAuthDead must prove is dead: the
	// guest's actual CURRENT credential. Under plain rotate that's the pool's
	// static password (never changes). Under scramble it has to be the fresh
	// value below instead — see the doc comment above.
	verifyPW := d.SSH.Password
	if d.Hardening.Scrambles() {
		pw := rand.Text()
		verifyPW = pw
		scrambleLine, err := perOS(goos, scrambleLineDarwin, scrambleLineLinux)
		if err != nil {
			return nil, fmt.Errorf("rotate: %w", err)
		}
		full += strings.ReplaceAll(scrambleLine, scramblePasswordPlaceholder, pw)
	}
	out, code, err = pg.c.Output(ctx, full)
	if err != nil {
		return nil, fmt.Errorf("rotate: installing cycle key: %w", err)
	}
	if code != 0 {
		return nil, fmt.Errorf("rotate: installing cycle key: exit %d: %s", code, out)
	}

	cfg := d.SSH
	cfg.Signer = signer
	cfg.HostKeys = hostKeys
	c, err := sshx.WaitFor(ctx, addr, cfg, d.interval())
	if err != nil {
		return nil, fmt.Errorf("rotate: reconnecting with cycle key: %w", err)
	}

	// The keyed redial proves the positive; now prove the negative. The
	// drop-in can silently lose — sshd is first-obtained-value-wins across
	// lexically-ordered includes, and image fleets ship their own auth
	// drop-ins — and a guest that still takes the password while reporting
	// SECURE_SSH ok is the silent un-hardening this state exists to kill.
	verifyCfg := d.SSH
	verifyCfg.Password = verifyPW
	if err := verifyPasswordAuthDead(ctx, addr, verifyCfg); err != nil {
		_ = c.Close()
		return nil, err
	}

	// All proven; the password session has done its one job per cycle. The new
	// Guest retains the keyed config (Signer + pinned HostKeys) so Redial and
	// HostKeys work against the hardened session (issue #39).
	_ = pg.c.Close()
	return &Guest{c: c, addr: addr, cfg: cfg, interval: d.interval(), goos: goos}, nil
}

// verifyPasswordAuthDead attempts password auth and requires ErrAuthRejected.
// It retries until ctx expiry because the linux reload is asynchronous (the
// systemctl exec returns when SIGHUP is sent, not when sshd has re-read its
// config). Acceptance is an immediate loud failure; any other outcome keeps
// polling and expires with the last evidence in the error.
//
// Residual: this exercises the "password" method only. A guest where only
// KbdInteractiveAuthentication survived the flip would pass — acceptable
// because both directives ride the same drop-in (they win or lose together);
// the ix verification's manual mid-cycle ssh exercises the full client stack.
func verifyPasswordAuthDead(ctx bounded.Context, addr string, cfg sshx.Config) error {
	var lastErr error
	for {
		pc, err := sshx.Dial(ctx, addr, cfg)
		switch {
		case err == nil:
			_ = pc.Close()
			lastErr = errors.New("guest accepted password auth")
		case errors.Is(err, sshx.ErrAuthRejected):
			return nil
		default:
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("rotate: password auth still alive after the flip (sshd config precedence?): %w (last attempt: %v)", ctx.Err(), lastErr)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// parseHostKeys parses captureHostKeys output: one authorized_keys-format
// line per host key. Any unparseable line is a loud failure, not a skip — a
// guest whose host keys can't be read can't be pinned, and proceeding
// unpinned would silently undo the defense.
func parseHostKeys(out []byte) ([]ssh.PublicKey, error) {
	var keys []ssh.PublicKey
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
		if err != nil {
			return nil, fmt.Errorf("parsing host key %q: %w", line, err)
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return nil, errors.New("guest returned no host keys")
	}
	return keys, nil
}

// PushRunnerTarball streams localPath's content to the guest's own
// runner-cache location — $HOME/runny-cache/<basename> (linux, cat >) or
// C:\runny-cache\<basename> (windows, a PowerShell stdin→file copy) — over
// the same already-hardened SSH session StartRunner will use next. Only
// called when vm.Machine.NeedsRunnerPush is true: darwin's virtiofs share is
// already live by the time PROVISION runs, so nothing needs pushing there.
func (g *Guest) PushRunnerTarball(ctx bounded.Context, localPath string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("opening runner tarball %s: %w", localPath, err)
	}
	defer f.Close()

	// base crosses into a shell/PowerShell command string below — reuse the
	// same trust-boundary guard provisionScript/StartRunner apply to the
	// tarball name (the charset carries no shell metacharacter and no `/`)
	// rather than assume every caller already validated it.
	base := filepath.Base(localPath)
	if !runnerAssetRE(g.goos).MatchString(base) {
		return fmt.Errorf("refusing to push runner asset with an unexpected name %q", base)
	}

	if g.goos == home.OSWindows {
		// scp's sink writes into an existing directory only; create it in a
		// separate quick exec first.
		out, code, err := g.c.Output(ctx, encodedCommand(fmt.Sprintf(`New-Item -Force -ItemType Directory -Path '%s' | Out-Null`, runnerCacheDirWindows)))
		if err != nil {
			return fmt.Errorf("creating runner cache dir: %w", err)
		}
		if code != 0 {
			return fmt.Errorf("creating runner cache dir: exit %d: %s", code, out)
		}
		st, err := f.Stat()
		if err != nil {
			return fmt.Errorf("pushing runner zip: %w", err)
		}
		dest := runnerCacheDirWindows + `\` + base
		out, code, err = g.c.RunWithInput(ctx, scpSinkCommand(dest), scpSource(base, st.Size(), f))
		if err != nil {
			return fmt.Errorf("pushing runner zip: %w", err)
		}
		if code != 0 {
			return fmt.Errorf("pushing runner zip: exit %d: %s", code, out)
		}
		return nil
	}

	cmd := fmt.Sprintf(`mkdir -p "$HOME/%s" && cat > "$HOME/%s/%s"`, runnerPushCacheDir, runnerPushCacheDir, base)
	out, code, err := g.c.RunWithInput(ctx, cmd, f)
	if err != nil {
		return fmt.Errorf("pushing runner tarball: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("pushing runner tarball: exit %d: %s", code, out)
	}
	return nil
}

// StartRunner stages and launches the runner for the pool's guest OS. The JIT
// config is handed to run.sh over the SSH session's stdin — the script reads it
// with `$(cat)` — and is NEVER interpolated into the command string. x/crypto
// folds cmd into its exec error on a server-side reject, and that error is
// recorded to cycle.json and served over the gRPC surface, so a secret inside
// cmd would leak to disk and the wire (the failure mode F1 exists to kill). The
// blob still ends up in the runner's argv on the guest (the shell expands
// `$(cat)` before exec) — that is the kill-marker StopRunner greps, unchanged;
// only the host-visible command string is now secret-free.
//
// env is the pool's guest_env: variables exported into the shell right before
// the runner launches, so run.sh and every job step inherit them. They ARE part
// of the command string (unlike the JIT), so guest_env is not for secrets.
// setup is the pool's guest_setup: shell commands run after the env exports,
// for system-level configuration guest_env can't express — same not-for-secrets
// caveat.
func (g *Guest) StartRunner(ctx context.Context, jit, goos, runnerTarball string, env map[string]string, setup []string, needsPush bool) (statemachine.Proc, error) {
	if goos == home.OSWindows {
		return g.startRunnerWindows(ctx, jit, runnerTarball)
	}
	script, err := provisionScript(goos, runnerTarball, env, setup, needsPush)
	if err != nil {
		return nil, err
	}
	p, err := g.c.Start(ctx, script, strings.NewReader(jit))
	if err != nil {
		return nil, fmt.Errorf("starting runner: %w", err)
	}
	return proc{p}, nil
}

// runnerAssetRE picks the trust-boundary guard for the guest's runner-asset
// basename: the tarball pattern for darwin/linux, the zip pattern for
// windows.
func runnerAssetRE(goos string) *regexp.Regexp {
	if goos == home.OSWindows {
		return runnerZipRE
	}
	return runnerTarballRE
}

// PullDiag fetches the tail of the runner's diagnostic logs — the
// post-mortem material TEARDOWN collects before destroying the guest.
func (g *Guest) PullDiag(ctx bounded.Context) ([]byte, error) {
	script := `for f in $HOME/runny-runner/_diag/*.log; do echo "==> $f <=="; tail -c 32768 "$f"; done 2>/dev/null`
	if g.goos == home.OSWindows {
		script = encodedCommand(pullDiagScriptWindows)
	}
	out, _, err := g.c.Output(ctx, script)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// PullDebugSession fetches the operator's session recording at teardown.
// Empty output (operator never connected) is returned as nil — the caller
// skips the artifact.
//
// Windows guests return nil unconditionally: no recorder mechanism is
// wired for Windows — the POSIX recorder is a script(1) wrapper installed
// alongside the debug key (installDebugKeyScript/debugRecorderDarwin/Linux),
// and Windows has no script(1) equivalent; recording there needs a
// forced-command transcription wrapper that doesn't exist yet.
//
// Uses a fresh connection for the same reason InstallAuthorizedKey does: the
// supervision client g.c carries the live runner Proc, and newSession sets a
// deadline on the SHARED net.Conn. Pulling over g.c during a forced teardown
// (stuck job, proc still alive) would fire that deadline on the runner's
// channel before proc.Kill() in step 2.
func (g *Guest) PullDebugSession(ctx bounded.Context) ([]byte, error) {
	if g.goos == home.OSWindows {
		return nil, nil
	}
	c, err := sshx.WaitFor(ctx, g.addr, g.cfg, g.interval)
	if err != nil {
		return nil, fmt.Errorf("debug session pull: %w: %w", statemachine.ErrGuestUnreachable, err)
	}
	defer func() { _ = c.Close() }()
	out, _, err := c.Output(ctx, "cat "+debugSessionLogFile+" 2>/dev/null || true")
	if err != nil {
		return nil, err
	}
	return out, nil
}

// StopRunner kills the runner listener tree and PROVES it dead (issue #39).
// Any nonzero exit or exec error = death unproven; the caller refuses the
// freeze/hold and fails into teardown.
func (g *Guest) StopRunner(ctx bounded.Context) error {
	script := stopRunnerScript
	if g.goos == home.OSWindows {
		script = encodedCommand(stopRunnerScriptWindows)
	}
	out, code, err := g.c.Output(ctx, script)
	if err != nil {
		return fmt.Errorf("stopping runner: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("stopping runner: kill unproven (exit %d): %s", code, strings.TrimSpace(string(out)))
	}
	return nil
}

// InstallAuthorizedKey appends one authorized_keys line and proves it landed
// (issue #39). line must be a server-canonicalized "type base64" form. A
// session-open failure (the command provably never reached the guest) is
// translated to statemachine.ErrGuestUnreachable; everything past session-open
// stays ambiguous.
func (g *Guest) InstallAuthorizedKey(ctx bounded.Context, line string) error {
	// Dial a fresh, short-lived connection for the install — never the
	// supervision client g.c. During a mid-job injection g.c carries the live
	// runner Proc, and newSession sets a deadline on the SHARED net.Conn, which
	// would fire on the runner's idle read and tear the job's connection down
	// (worst in the stuck-job case this feature targets). A separate conn keeps
	// the install's deadline on its own socket. Same per-cycle credentials and
	// host pins (g.cfg) the Rotate/Redial reconnects already use; a dial failure
	// is "provably never reached the guest" → ErrGuestUnreachable.
	c, err := sshx.WaitFor(ctx, g.addr, g.cfg, g.interval)
	if err != nil {
		return fmt.Errorf("installing debug key: %w: %w", statemachine.ErrGuestUnreachable, err)
	}
	defer func() { _ = c.Close() }()

	var (
		out  []byte
		code int
	)
	if g.goos == home.OSWindows {
		// No recorder is wired for Windows guests — see
		// PullDebugSession's doc comment for the technical reason. Log loudly
		// rather than silently hand out an unrecorded operator session.
		slog.Warn("windows debug session: transcript capture is unsupported, the operator's session will not be recorded")
		lineEsc := psQuote(line)
		script := fmt.Sprintf(installDebugKeyScriptWindows, lineEsc, lineEsc)
		out, code, err = c.Output(ctx, encodedCommand(script))
	} else {
		recorder, rErr := perOS(g.goos, debugRecorderDarwin, debugRecorderLinux)
		if rErr != nil {
			return fmt.Errorf("installing debug key: %w", rErr)
		}
		script := fmt.Sprintf(installDebugKeyScript, recorder, line, line)
		out, code, err = c.Output(ctx, script)
	}
	if err != nil {
		if errors.Is(err, sshx.ErrSessionOpen) {
			return fmt.Errorf("installing debug key: %w: %w", statemachine.ErrGuestUnreachable, err)
		}
		return fmt.Errorf("installing debug key: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("installing debug key: read-back failed (exit %d): %s", code, strings.TrimSpace(string(out)))
	}
	return nil
}

// HostKeys returns the guest's pinned host keys in known_hosts form (issue
// #39): "<addr-host> <type> <base64>". Empty when the guest is unhardened
// (ssh_hardening: off, no pins captured).
func (g *Guest) HostKeys() []string {
	if len(g.cfg.HostKeys) == 0 {
		return nil
	}
	host := g.addr
	if h, _, err := net.SplitHostPort(g.addr); err == nil {
		host = h
	}
	out := make([]string, 0, len(g.cfg.HostKeys))
	for _, k := range g.cfg.HostKeys {
		line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k)))
		out = append(out, host+" "+line)
	}
	return out
}

// Redial re-establishes the session after a transport death (a guest reboot
// mid-DEBUG, issue #39), reusing the retained config (Signer + pinned host
// keys). NEVER called during JOB (decision 18). The old client is best-effort
// closed; on success the client is swapped.
func (g *Guest) Redial(ctx bounded.Context) error {
	c, err := sshx.WaitFor(ctx, g.addr, g.cfg, g.interval)
	if err != nil {
		return fmt.Errorf("redial: %w", err)
	}
	_ = g.c.Close()
	g.c = c
	return nil
}

func (g *Guest) Close() error { return g.c.Close() }

// proc adapts sshx.Proc to statemachine.Proc.
type proc struct{ p *sshx.Proc }

func (p proc) Lines() <-chan string { return p.p.Lines }
func (p proc) Wait() (int, error)   { return p.p.Wait() }
func (p proc) Kill()                { p.p.Kill() }
