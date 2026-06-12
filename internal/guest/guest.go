// Package guest adapts sshx sessions to the state machine's Guest interface:
// it knows how a cirruslabs guest stages the actions runner from the virtiofs
// cache share and launches run.sh with a JIT config.
package guest

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/bojanrajkovic/runny/internal/bounded"
	"github.com/bojanrajkovic/runny/internal/sshx"
	"github.com/bojanrajkovic/runny/internal/statemachine"
)

// Dialer implements statemachine.Dialer over sshx.
type Dialer struct {
	SSH sshx.Config
	// RetryInterval between connection attempts (default 2s).
	RetryInterval time.Duration
}

func (d Dialer) interval() time.Duration {
	if d.RetryInterval != 0 {
		return d.RetryInterval
	}
	return 2 * time.Second
}

func (d Dialer) WaitFor(ctx bounded.Context, addr string) (statemachine.Guest, error) {
	c, err := sshx.WaitFor(ctx, addr, d.SSH, d.interval())
	if err != nil {
		return nil, err
	}
	return &Guest{c: c}, nil
}

// Guest is one authenticated session into a booted runner VM.
type Guest struct {
	c *sshx.Client
}

// The rotation scripts install the per-cycle public key and shut password
// auth off (ADR-0013). They rely on the image contract the provision scripts
// already demand — passwordless sudo, an sshd_config that includes
// sshd_config.d, sshd recent enough for KbdInteractiveAuthentication (8.7+).
// An image missing any of these fails the exec or the post-flip verification
// loudly, by design.
//
// The drop-in must sort FIRST in sshd_config.d: sshd takes the first
// obtained value per keyword, and Include globs expand in lexical order —
// stock images ship later-sorting drop-ins that would win over a 99- name
// (ubuntu cloud images: 50-cloud-init.conf with "PasswordAuthentication
// yes"; macOS: 100-macos.conf). 00- beats both; verifyPasswordAuthDead
// catches any image where even that loses.
const rotateScriptBase = `set -e
umask 077
mkdir -p "$HOME/.ssh"
echo '%s' >> "$HOME/.ssh/authorized_keys"
printf 'PasswordAuthentication no\nKbdInteractiveAuthentication no\n' | sudo tee /etc/ssh/sshd_config.d/00-runny.conf >/dev/null
`

// linux: reload, NOT restart — reload keeps the established session (this
// one) and the listener alive while re-reading config. Debian-family units
// are named ssh, RHEL-family sshd; try both.
const rotateScriptLinux = rotateScriptBase + `sudo systemctl reload ssh || sudo systemctl reload sshd
`

// darwin: no reload — launchd socket-activates sshd per connection, so each
// connection's sshd reads the config fresh at spawn.
const rotateScriptDarwin = rotateScriptBase

// captureHostKeys reads every host public key the guest may present during
// key exchange. All of them: the host-key algorithm is negotiated per
// connection, so the pin set must cover whatever sshd offers
// (sshx.Config.HostKeys). The .pub files are world-readable; no sudo.
// awk 1, not cat: cat concatenates, so a .pub missing its trailing newline
// would merge two keys into one line and one pin would silently vanish
// (ParseAuthorizedKey reads the second key as the first one's comment).
const captureHostKeys = `awk 1 /etc/ssh/ssh_host_*_key.pub`

// Rotate hardens an authenticated session (ADR-0013): mint an in-memory
// per-cycle ed25519 key, capture the guest's host keys, install the key and
// disable password auth over the existing password session, reconnect
// authenticated by the key with the host keys pinned — then PROVE the
// password is dead by attempting it and requiring rejection. The private key
// never touches disk and dies with this process.
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

	script := rotateScriptDarwin
	if goos == "linux" {
		script = rotateScriptLinux
	}
	pubLine := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	out, code, err = pg.c.Output(ctx, fmt.Sprintf(script, pubLine))
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
	if err := verifyPasswordAuthDead(ctx, addr, d.SSH); err != nil {
		_ = c.Close()
		return nil, err
	}

	// All proven; the password session has done its one job per cycle.
	_ = pg.c.Close()
	return &Guest{c: c}, nil
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

// The provision scripts stage the runner and exec run.sh, per guest OS.
//
// The runner ALWAYS comes from our cache share, into a runny-owned dir —
// cirruslabs images ship a preinstalled ~/actions-runner whose version rots
// (a bundled v2.332.0 got "deprecated and cannot receive messages" from the
// broker), and JIT runners cannot self-update. Never trust the image's copy.
//
// Exit 78 (EX_CONFIG) = cache share missing the tarball — a host-side
// problem the post-mortem will show verbatim.

// darwin: the share appears at the automount path (macOS automounts tagged
// virtiofs shares) or gets mounted explicitly by tag; handle both.
//
// An SSH exec is a non-login shell, so macOS hands it a minimal PATH
// (/usr/bin:/bin:/usr/sbin:/sbin) with /etc/zprofile (and path_helper) never
// sourced — which drops /usr/local/bin, where pkg installers like the AWS CLI
// symlink, and Homebrew. The runner inherits this PATH and passes it to every
// job step, so a step that installs a tool into /usr/local/bin then can't run
// it ("aws: command not found" right after a successful install). Rebuild the
// PATH a normal login session has, once, before launching the runner.
const provisionScriptDarwin = `set -e
eval "$(/usr/libexec/path_helper -s)"
[ -x /opt/homebrew/bin/brew ] && eval "$(/opt/homebrew/bin/brew shellenv)" || true
CACHE="/Volumes/My Shared Files"
if [ ! -d "$CACHE" ]; then
  sudo mkdir -p /Volumes/runny-cache 2>/dev/null || true
  sudo mount_virtiofs runny-cache /Volumes/runny-cache 2>/dev/null || true
  CACHE="/Volumes/runny-cache"
fi
TARBALL="$(ls "$CACHE"/actions-runner-osx-arm64-*.tar.gz 2>/dev/null | head -1)"
if [ -z "$TARBALL" ]; then echo "runny: no actions-runner tarball in cache share $CACHE" >&2; exit 78; fi
RUNNER_DIR="$HOME/runny-runner"
rm -rf "$RUNNER_DIR" && mkdir -p "$RUNNER_DIR" && cd "$RUNNER_DIR"
tar -xzf "$TARBALL"
exec ./run.sh --jitconfig '%s'
`

// linux: explicit virtiofs mount; installdependencies.sh covers images
// missing libicu et al (idempotent, tolerated offline when deps exist).
const provisionScriptLinux = `set -e
CACHE=/mnt/runny-cache
sudo mkdir -p "$CACHE"
mountpoint -q "$CACHE" || sudo mount -t virtiofs runny-cache "$CACHE"
TARBALL="$(ls "$CACHE"/actions-runner-linux-arm64-*.tar.gz 2>/dev/null | head -1)"
if [ -z "$TARBALL" ]; then echo "runny: no actions-runner tarball in cache share $CACHE" >&2; exit 78; fi
RUNNER_DIR="$HOME/runny-runner"
rm -rf "$RUNNER_DIR" && mkdir -p "$RUNNER_DIR" && cd "$RUNNER_DIR"
tar -xzf "$TARBALL"
sudo ./bin/installdependencies.sh >/dev/null 2>&1 || true
exec ./run.sh --jitconfig '%s'
`

// StartRunner stages and launches the runner for the pool's guest OS.
func (g *Guest) StartRunner(ctx context.Context, jit, goos string) (statemachine.Proc, error) {
	script := provisionScriptDarwin
	if goos == "linux" {
		script = provisionScriptLinux
	}
	p, err := g.c.Start(ctx, fmt.Sprintf(script, jit))
	if err != nil {
		return nil, fmt.Errorf("starting runner: %w", err)
	}
	return proc{p}, nil
}

// PullDiag fetches the tail of the runner's diagnostic logs — the
// post-mortem material TEARDOWN collects before destroying the guest.
func (g *Guest) PullDiag(ctx bounded.Context) ([]byte, error) {
	out, _, err := g.c.Output(ctx,
		`for f in $HOME/runny-runner/_diag/*.log; do echo "==> $f <=="; tail -c 32768 "$f"; done 2>/dev/null`)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (g *Guest) Close() error { return g.c.Close() }

// proc adapts sshx.Proc to statemachine.Proc.
type proc struct{ p *sshx.Proc }

func (p proc) Lines() <-chan string { return p.p.Lines }
func (p proc) Wait() (int, error)   { return p.p.Wait() }
func (p proc) Kill()                { p.p.Kill() }
