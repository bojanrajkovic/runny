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
	"io"
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

// runStep runs cmd on the guest and collapses the exec-error/nonzero-exit
// check every discarded-output call site otherwise repeats inline. input nil
// runs cmd via Output; non-nil runs it via RunWithInput (a byte-stream push).
func (g *Guest) runStep(ctx bounded.Context, what, cmd string, input io.Reader) error {
	_, err := g.runStepOutput(ctx, what, cmd, input)
	return err
}

// runStepOutput is runStep for a step whose stdout the caller still needs on
// success — capture-host-keys parses it. Not for PullDiag/PullDebugSession
// (both discard the exit code entirely; they're best-effort tails, not a
// step that fails on a nonzero exit) or InstallAuthorizedKey (runs over its
// own short-lived connection, not g.c, and reclassifies a session-open exec
// error into ErrGuestUnreachable — a distinction this generic exit-code
// check can't express).
func (g *Guest) runStepOutput(ctx bounded.Context, what, cmd string, input io.Reader) ([]byte, error) {
	var (
		out  []byte
		code int
		err  error
	)
	if input != nil {
		out, code, err = g.c.RunWithInput(ctx, cmd, input)
	} else {
		out, code, err = g.c.Output(ctx, cmd)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	if code != 0 {
		return nil, fmt.Errorf("%s: exit %d: %s", what, code, out)
	}
	return out, nil
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

	out, err := pg.runStepOutput(ctx, "rotate: capturing host keys", captureHostKeys, nil)
	if err != nil {
		return nil, err
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
	if err := pg.runStep(ctx, "rotate: installing cycle key", full, nil); err != nil {
		return nil, err
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
// the on-host verification's manual mid-cycle ssh exercises the full client stack.
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
		mkdirCmd := encodedCommand(fmt.Sprintf(`New-Item -Force -ItemType Directory -Path '%s' | Out-Null`, runnerCacheDirWindows))
		if err := g.runStep(ctx, "creating runner cache dir", mkdirCmd, nil); err != nil {
			return err
		}
		st, err := f.Stat()
		if err != nil {
			return fmt.Errorf("pushing runner zip: %w", err)
		}
		dest := runnerCacheDirWindows + `\` + base
		return g.runStep(ctx, "pushing runner zip", scpSinkCommand(dest), scpSource(base, st.Size(), f))
	}

	cmd := fmt.Sprintf(`mkdir -p "$HOME/%s" && cat > "$HOME/%s/%s"`, runnerPushCacheDir, runnerPushCacheDir, base)
	return g.runStep(ctx, "pushing runner tarball", cmd, f)
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
func (g *Guest) StartRunner(ctx context.Context, jit, runnerTarball string, env map[string]string, setup []string, needsPush bool) (statemachine.Proc, error) {
	if g.goos == home.OSWindows {
		return g.startRunnerWindows(ctx, jit, runnerTarball)
	}
	script, err := provisionScript(g.goos, runnerTarball, env, setup, needsPush)
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

// PullDiag fetches the runner's diagnostic logs whole — the post-mortem
// material TEARDOWN collects before destroying the guest. Whole rather than
// tailed: the guest is about to be destroyed, so whatever is not pulled here
// is gone, and which part of a log explains a failure is not knowable in
// advance. sshx.Output's cap is the backstop against a runaway guest.
func (g *Guest) PullDiag(ctx bounded.Context) ([]byte, error) {
	script := `for f in $HOME/runny-runner/_diag/*.log; do echo "==> $f <=="; cat "$f"; done 2>/dev/null`
	if g.goos == home.OSWindows {
		script = encodedCommand(pullDiagScriptWindows)
	}
	// Partial output is returned alongside the error rather than dropped: a
	// pull that exhausted its deadline mid-transfer still holds everything
	// that arrived, and the guest is about to be destroyed.
	out, _, err := g.c.Output(ctx, script)
	return out, err
}

// PullDebugSession fetches the operator's session recording at teardown.
// Empty output (operator never connected) is returned as nil — the caller
// skips the artifact.
//
// Windows guests: see debugRecorderScriptWindows's doc comment for the two
// recording mechanisms (one per SSH usage shape) and their proven limits.
//
// Uses a fresh connection for the same reason InstallAuthorizedKey does: the
// supervision client g.c carries the live runner Proc, and newSession sets a
// deadline on the SHARED net.Conn. Pulling over g.c during a forced teardown
// (stuck job, proc still alive) would fire that deadline on the runner's
// channel before proc.Kill() in step 2.
func (g *Guest) PullDebugSession(ctx bounded.Context) ([]byte, error) {
	c, err := sshx.WaitFor(ctx, g.addr, g.cfg, g.interval)
	if err != nil {
		return nil, fmt.Errorf("debug session pull: %w: %w", statemachine.ErrGuestUnreachable, err)
	}
	defer func() { _ = c.Close() }()
	script := "cat " + debugSessionLogFile + " 2>/dev/null || true"
	if g.goos == home.OSWindows {
		script = encodedCommand(pullDebugSessionScriptWindows)
	}
	out, _, err := c.Output(ctx, script)
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
	return g.runStep(ctx, "stopping runner: kill unproven", script, nil)
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
		// Not Warn: this fires on every windows debug key install, and it's a
		// known, documented capability boundary (debugRecorderScriptWindows's
		// doc comment), not a fault — Warn-per-install would be cry-wolf noise
		// that dilutes real anomalies. Info keeps it discoverable in the
		// daemon's own logs for an operator later wondering why a build tool's
		// output is missing from an interactive session's transcript.
		slog.Info("windows debug session: interactive-branch recording does not capture native/external program output; run non-interactively for guaranteed capture")
		lineEsc := psQuote(line)
		script := fmt.Sprintf(installDebugKeyScriptWindows, lineEsc, debugRecorderScriptWindows)
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
