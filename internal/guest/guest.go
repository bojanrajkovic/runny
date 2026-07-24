// Package guest adapts sshx sessions to the state machine's Guest interface:
// it knows how a cirruslabs guest stages the actions runner from the virtiofs
// cache share and launches run.sh with a JIT config.
package guest

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf16"

	"golang.org/x/crypto/ssh"

	"github.com/bojanrajkovic/runny/internal/bounded"
	"github.com/bojanrajkovic/runny/internal/home"
	"github.com/bojanrajkovic/runny/internal/sshx"
	"github.com/bojanrajkovic/runny/internal/statemachine"
)

// windowsPrepTimeout bounds the extract + JIT-delivery steps StartRunner runs
// on a windows guest before handing off to the watcher Proc. Fixed rather
// than pool-configurable: measured windows boot-to-SSH is ~6s, and these
// steps are a local unzip plus two small SSH execs, so a generous fixed
// bound carries the same margin the rest of the cycle's global deadlines do
// without adding a new per-OS deadline knob.
const windowsPrepTimeout = 60 * time.Second

// encodedCommand renders script as a `powershell -EncodedCommand` invocation:
// UTF-16LE-encode, then base64. This is the only reliable way to hand a
// multi-line PowerShell script through ssh/cmd.exe/PowerShell 5.1's stacked
// quoting rules — the target shell for a windows guest's default SSH session
// is cmd.exe, and anything beyond a trivial one-liner mangles under
// cmd-then-PS-then-argv quoting. No secret ever goes through this path: the
// JIT config crosses over stdin, exactly like the POSIX `$(cat)` pattern.
//
// The $ProgressPreference prefix is hardware-earned: PowerShell 5.1's
// console-less host serializes its progress stream as a `#< CLIXML` blob on
// stderr (module autoload emits "Preparing modules for first use" on a fresh
// guest), which pollutes any output a caller parses. Silencing progress for
// every guest script at this one seam kills the whole class. Real errors
// still arrive CLIXML-wrapped on stderr — ugly but loud; exit codes stay the
// failure signal.
func encodedCommand(script string) string {
	u16 := utf16.Encode([]rune("$ProgressPreference='SilentlyContinue'; " + script))
	buf := make([]byte, len(u16)*2)
	for i, v := range u16 {
		binary.LittleEndian.PutUint16(buf[i*2:], v)
	}
	return "powershell -NoProfile -NonInteractive -EncodedCommand " + base64.StdEncoding.EncodeToString(buf)
}

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

// psQuote escapes s for embedding inside a PowerShell single-quoted string
// literal ('...'): PS1's own escape convention is doubling the embedded
// quote, distinct from POSIX's backslash-quote. Used at every windows
// trust-boundary site where a Go string is spliced into a PS script.
func psQuote(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// administratorsAuthorizedKeysPath is the ONE authorized-keys file Windows
// OpenSSH honors for a member of the Administrators group (the image's baked
// Administrator account).
const administratorsAuthorizedKeysPath = `C:\ProgramData\ssh\administrators_authorized_keys`

// psAppendAuthorizedKeyLine renders the fragment that appends valueExpr (a PS
// expression — a variable or a literal) to administrators_authorized_keys
// and fixes its ACL, shared by rotate's cycle-key install and the debug-key
// install so the ACL fix has exactly one home. The ACL fix is
// security-load-bearing, not cosmetic: sshd silently ignores the whole file
// unless its inherited permissions are stripped down to SYSTEM and
// Administrators only, and doesn't say why the key never worked. icacls
// failing is a loud `exit 2`, not swallowed by `| Out-Null` — a failed ACL
// fix is the one condition under which a "successful" key append does
// nothing, and `| Out-Null` on its own only discards icacls's stdout, not
// $LASTEXITCODE.
func psAppendAuthorizedKeyLine(valueExpr string) string {
	return fmt.Sprintf(`Add-Content -Path '%s' -Value %s
icacls '%s' /inheritance:r /grant "SYSTEM:F" /grant "BUILTIN\Administrators:F" | Out-Null
if ($LASTEXITCODE -ne 0) { exit 2 }
`, administratorsAuthorizedKeysPath, valueExpr, administratorsAuthorizedKeysPath)
}

// scpSinkCommand is the exec command for streaming one file into the guest:
// Windows OpenSSH ships scp.exe, and its sink mode is the one byte channel
// with no shell or PowerShell host in the path. A PowerShell stdin-copy
// ([Console]::OpenStandardInput) does NOT work here — hardware-proven: the
// PS 5.1 console-less host interposes on redirected stdin, and streaming
// binary through it either wedges the session until the state deadline or
// gets the connection killed by the guest. scp.exe is a native binary, so
// the command survives any DefaultShell (cmd or powershell) unchanged.
func scpSinkCommand(remotePath string) string {
	return "scp -t " + remotePath
}

// scpSource frames r as a single-file SCP source stream for scpSinkCommand:
// the C-record header, the bytes, and the NUL terminator. The sink's acks
// are deliberately not read — this is a blind single-file stream, and the
// session's exit code is the success signal (scp -t exits nonzero with a
// message on stderr for a short write, a bad path, or a framing error).
func scpSource(name string, size int64, r io.Reader) io.Reader {
	header := fmt.Sprintf("C0644 %d %s\n", size, name)
	return io.MultiReader(strings.NewReader(header), r, bytes.NewReader([]byte{0}))
}

// The rotation scripts install the per-cycle public key and shut password
// auth off. They rely on the image contract the provision scripts
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

// scramblePasswordPlaceholder is substituted via strings.ReplaceAll, not a
// fmt verb — a plain substring swap needs no escaping discipline, matching
// provisionScript's __RUNNER_TARBALL__ placeholder below.
const scramblePasswordPlaceholder = "__RUNNY_SCRAMBLE_PASSWORD__"

// scrambleLineLinux / scrambleLineDarwin set a fresh, never-disclosed
// password for the just-authenticated account (ssh_hardening: scramble,
// issue #210), appended to the rotate script so it lands in the same exec as
// the key install — one round-trip, one set -e failure path. A scramble
// failure aborts after PasswordAuthentication is already off, so it degrades
// to plain "rotate" behavior rather than a worse state.
//
// The username comes from `id -un` on the guest, not a Go-level value
// substituted in: the only thing interpolated into either line is the
// random password, so there is nothing here for a misconfigured SSHUser to
// inject into.
//
// Residual, both OSes: the whole rotate script — this line included — is
// delivered as one SSH exec, which sshd runs as `<shell> -c "<script>"`, so
// the password is live in that shell's argv (`ps`/`/proc/<pid>/cmdline`) for
// the exec's full duration, not just chpasswd's own process. Same residual
// class the JIT config already accepts on the guest side (StartRunner's
// comment, below) — accepted here because this runs during SECURE_SSH,
// before any operator debug key could exist to read it.
const scrambleLineLinux = `printf '%s:%s\n' "$(id -un)" '` + scramblePasswordPlaceholder + `' | sudo chpasswd
`

const scrambleLineDarwin = `sudo dscl . -passwd "/Users/$(id -un)" '` + scramblePasswordPlaceholder + `'
`

// captureHostKeysWindowsScript is Windows sshd's equivalent of captureHostKeys:
// print every host public key so the reconnect can pin the full offered set.
const captureHostKeysWindowsScript = `Get-ChildItem 'C:\ProgramData\ssh\ssh_host_*_key.pub' | ForEach-Object { Get-Content $_.FullName }`

// rotateScriptWindowsTemplate installs the per-cycle public key into
// Windows OpenSSH's administrators_authorized_keys, disables password auth,
// and restarts sshd — the windows equivalent of rotateScriptBase.
//
// administrators_authorized_keys is the ONE authorized-keys file Windows
// OpenSSH honors for a member of the Administrators group (the image's
// baked Administrator account), and it is silently ignored unless its ACL
// strips inherited permissions down to SYSTEM and Administrators only —
// sshd refuses to trust a file any other principal can write, and doesn't
// say why the key never worked.
//
// PasswordAuthentication no is PREPENDED, not appended: sshd_config is
// first-match-wins, and the stock Windows sshd_config ends with a
// `+"`Match Group administrators`"+` block — appending after it would land
// inside that block and lose to whatever precedes it, or worse, silently
// apply only to a subset of matches. Prepending guarantees the directive is
// the first one sshd reads, for every connection.
//
// Restarting sshd from inside the session that is issuing the restart would
// kill this very session — Windows sshd connections are children of the
// service process, unlike systemd's per-connection reload on Linux. The
// restart is handed to a DETACHED process that sleeps briefly before
// restarting the service, so this script's own exit status (and the
// session carrying it) survives to be read back.
var rotateScriptWindowsTemplate = `$pub = '%s'
` + psAppendAuthorizedKeyLine("$pub") + `$cfgPath = 'C:\ProgramData\ssh\sshd_config'
$existing = Get-Content -Path $cfgPath -Raw
Set-Content -Path $cfgPath -Value ("PasswordAuthentication no` + "`r`n" + `" + $existing) -NoNewline
%sStart-Process powershell -WindowStyle Hidden -ArgumentList '-NoProfile','-Command','Start-Sleep -Seconds 1; Restart-Service sshd' | Out-Null
`

// scrambleLineWindowsTemplate randomizes the baked Administrator account's
// password (ssh_hardening: scramble). Set-LocalUser, not `+"`net user`"+`: net
// user prompts interactively for any password over 14 characters instead of
// accepting one on the command line, which would hang the exec forever.
const scrambleLineWindowsTemplate = `Set-LocalUser -Name Administrator -Password (ConvertTo-SecureString '%s' -AsPlainText -Force)
`

// captureHostKeys reads every host public key the guest may present during
// key exchange. All of them: the host-key algorithm is negotiated per
// connection, so the pin set must cover whatever sshd offers
// (sshx.Config.HostKeys). The .pub files are world-readable; no sudo.
// awk 1, not cat: cat concatenates, so a .pub missing its trailing newline
// would merge two keys into one line and one pin would silently vanish
// (ParseAuthorizedKey reads the second key as the first one's comment).
const captureHostKeys = `awk 1 /etc/ssh/ssh_host_*_key.pub`

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

// rotateWindows is Rotate's windows branch: same mint → capture → install →
// reconnect → prove-the-negative choreography as the POSIX path, but every
// step is a PowerShell script against Windows OpenSSH's own config surface
// (administrators_authorized_keys, sshd_config, the sshd service) instead of
// a POSIX one-liner. See rotateScriptWindowsTemplate's doc comment for the
// ACL and prepend traps, and the detached-restart doc comment there for why
// the service restart can't run inline.
func (d Dialer) rotateWindows(ctx bounded.Context, addr string, pg *Guest, signer ssh.Signer) (statemachine.Guest, error) {
	out, code, err := pg.c.Output(ctx, encodedCommand(captureHostKeysWindowsScript))
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

	pubLine := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	pubEsc := psQuote(pubLine)

	verifyPW := d.SSH.Password
	scramble := ""
	if d.Hardening.Scrambles() {
		pw := rand.Text()
		verifyPW = pw
		scramble = fmt.Sprintf(scrambleLineWindowsTemplate, psQuote(pw))
	}
	script := fmt.Sprintf(rotateScriptWindowsTemplate, pubEsc, scramble)

	out, code, err = pg.c.Output(ctx, encodedCommand(script))
	if err != nil {
		return nil, fmt.Errorf("rotate: installing cycle key: %w", err)
	}
	if code != 0 {
		return nil, fmt.Errorf("rotate: installing cycle key: exit %d: %s", code, out)
	}

	cfg := d.SSH
	cfg.Signer = signer
	cfg.HostKeys = hostKeys
	// The restart is async (a detached process sleeping 1s before
	// Restart-Service): the old sshd may still be answering when this dials,
	// so WaitFor's own retry loop is what carries the reconnect across the
	// flip, exactly as it does for the POSIX reload/socket-activation cases.
	c, err := sshx.WaitFor(ctx, addr, cfg, d.interval())
	if err != nil {
		return nil, fmt.Errorf("rotate: reconnecting with cycle key: %w", err)
	}

	verifyCfg := d.SSH
	verifyCfg.Password = verifyPW
	if err := verifyPasswordAuthDead(ctx, addr, verifyCfg); err != nil {
		_ = c.Close()
		return nil, err
	}

	_ = pg.c.Close()
	return &Guest{c: c, addr: addr, cfg: cfg, interval: d.interval(), goos: home.OSWindows}, nil
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
// The share is this cycle's own per-slot mount, holding exactly the one tarball
// it cloned before boot. The script still stages that EXACT tarball by basename
// (substituted for __RUNNER_TARBALL__), not a `ls | head -1` glob: defense in
// depth that keeps the on-disk record honest (the staged version matches the
// RunnerVersion recorded for the cycle) and the cache-miss diagnostic precise,
// rather than a lexical pick.
//
// Exit 78 (EX_CONFIG) = the mount is missing this tarball — a host-side
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
# Surface the guest clock: a stale RTC breaks runner registration with an
# opaque expired-token error.
echo "runny: provision-clock $(date -u +%Y-%m-%dT%H:%M:%SZ)"
CACHE="/Volumes/My Shared Files"
if [ ! -d "$CACHE" ]; then
  sudo mkdir -p /Volumes/runny-cache 2>/dev/null || true
  sudo mount_virtiofs runny-cache /Volumes/runny-cache 2>/dev/null || true
  CACHE="/Volumes/runny-cache"
fi
TARBALL="$CACHE/__RUNNER_TARBALL__"
if [ ! -f "$TARBALL" ]; then echo "runny: runner tarball __RUNNER_TARBALL__ not in cache share $CACHE" >&2; exit 78; fi
RUNNER_DIR="$HOME/runny-runner"
rm -rf "$RUNNER_DIR" && mkdir -p "$RUNNER_DIR" && cd "$RUNNER_DIR"
tar -xzf "$TARBALL"
exec ./run.sh --jitconfig "$(cat)"
`

// linuxProvisionPrelude is the clock tripwire shared by every linux variant
// (same reasoning as the darwin script's own copy).
const linuxProvisionPrelude = `set -e
echo "runny: provision-clock $(date -u +%Y-%m-%dT%H:%M:%SZ)"
`

// linuxProvisionBody is shared by every linux variant once CACHE is set:
// stage the exact tarball, extract it, and exec run.sh. Only how CACHE gets
// populated differs between variants (linuxCacheMount / linuxCachePushed
// below) — kept as the one thing that varies so the two variants can't drift
// out of sync on everything else, the way their error-message wording once did.
const linuxProvisionBody = `TARBALL="$CACHE/__RUNNER_TARBALL__"
if [ ! -f "$TARBALL" ]; then echo "runny: runner tarball __RUNNER_TARBALL__ not in cache $CACHE" >&2; exit 78; fi
RUNNER_DIR="$HOME/runny-runner"
rm -rf "$RUNNER_DIR" && mkdir -p "$RUNNER_DIR" && cd "$RUNNER_DIR"
tar -xzf "$TARBALL"
sudo ./bin/installdependencies.sh >/dev/null 2>&1 || true
exec ./run.sh --jitconfig "$(cat)"
`

// linuxCacheMount: explicit virtiofs mount; installdependencies.sh (in
// linuxProvisionBody) covers images missing libicu et al (idempotent,
// tolerated offline when deps exist).
const linuxCacheMount = `CACHE=/mnt/runny-cache
sudo mkdir -p "$CACHE"
mountpoint -q "$CACHE" || sudo mount -t virtiofs runny-cache "$CACHE"
`

// provisionScriptLinux: the live-share variant (darwin's virtiofs-equivalent
// on the guest side).
const provisionScriptLinux = linuxProvisionPrelude + linuxCacheMount + linuxProvisionBody

// runnerPushCacheDir is where PushRunnerTarball stages the tarball, relative
// to $HOME, when the boot backend has no live share device (windows host —
// see hcs_windows.go's NeedsRunnerPush doc comment for why). Under $HOME
// rather than /mnt like linuxCacheMount's CACHE: the push runs over the
// already-established SSH session as the same non-root user that owns its
// own home dir, so no sudo is needed to create it. linuxCachePushed derives
// its CACHE line from this constant rather than restating it, so the two
// can't drift the way they briefly could when both were separate literals.
const runnerPushCacheDir = "runny-cache"

// linuxCachePushed: no virtiofs-equivalent share device works from a bare
// compute system -- PushRunnerTarball stages the tarball at $HOME/runny-cache
// before this script runs, so there is no mount step here.
const linuxCachePushed = `CACHE="$HOME/` + runnerPushCacheDir + `"
`

// provisionScriptLinuxPushed: the pushed-cache variant (windows HOST, bare
// compute Linux guest — see hcsMachine.NeedsRunnerPush).
const provisionScriptLinuxPushed = linuxProvisionPrelude + linuxCachePushed + linuxProvisionBody

// runnerCacheDirWindows is where PushRunnerTarball stages the runner zip on a
// windows GUEST, and where StartRunner's extract step reads it from — always
// pushed (a windows guest only ever boots on the HCS host, whose
// NeedsRunnerPush is unconditionally true), so there is no live-share
// variant to keep in sync with, unlike the linux push/mount split above.
const runnerCacheDirWindows = `C:\runny-cache`

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

// runnerDirWindows is where the runner zip is extracted to on a windows
// guest — the fixed layout the image's launcher polls (jitPathWindows) and
// runs the listener out of.
const runnerDirWindows = `C:\actions-runner`

// jitPendingPathWindows / jitPathWindows: the JIT config lands at the
// .tmp path first, then is renamed into place — write-then-rename so the
// image's launcher (polling jitPathWindows) can never observe a partial
// file, the windows equivalent of the POSIX path's atomic `$(cat)` handoff.
const (
	jitPendingPathWindows = runnerDirWindows + `\.jitconfig.tmp`
	jitPathWindows        = runnerDirWindows + `\.jitconfig`
)

// runnerLogPathWindows / runnerExitPathWindows are the launcher's own
// contract (baked into the published image): it redirects the runner's
// output to the log path and writes the runner's exit code to the exit path
// once it exits. watcherScriptWindows tails the former and exits with the
// latter.
const (
	runnerLogPathWindows  = `C:\runny\runner.log`
	runnerExitPathWindows = `C:\runny\runner-exit.txt`
)

// watcherScriptWindows is the windows equivalent of run.sh: not a launch,
// but a watch. The image's own scheduled-task launcher (in the AutoLogon
// desktop session) is what actually starts the runner once .jitconfig
// appears — this script polls the launcher's log/exit-code contract and
// streams it back as the returned Proc's Lines(), so the FSM's
// "Listening for Jobs" watch and job/exit tracking work unchanged. It
// tolerates the log file not existing yet (the launcher may not have picked
// up .jitconfig at the moment this starts) by simply continuing to poll —
// PROVISION's own deadline is what bounds a launcher that never starts.
//
// Drain writes the raw log bytes straight to the standard-output stream
// rather than through [Console]::Out (a TextWriter bound to the console's
// OEM codepage): re-encoding through it would mangle non-ASCII runner
// output, and a drain boundary landing mid-multibyte-sequence would corrupt
// it further. Writing the bytes verbatim leaves decoding to the host side,
// exactly like every POSIX Proc's output already does.
//
// The exit-code file's content is guarded by an integer regex before `exit`
// casts it — defense in depth: the 250ms settle-then-redrain before reading
// it already makes an empty/partial read practically unreachable, but a
// non-numeric read falls through to another poll rather than crashing the
// watcher on a bad cast. The sign is part of the match: a crashed process
// reports a negative 32-bit exit code (an NTSTATUS like -1073741510), and a
// digits-only guard would loop forever on exactly the exits that most need
// reporting.
const watcherScriptWindows = `$log = '` + runnerLogPathWindows + `'
$exitFile = '` + runnerExitPathWindows + `'
$pos = 0
$stdout = [Console]::OpenStandardOutput()
function Drain {
  if (Test-Path $log) {
    $len = (Get-Item $log).Length
    if ($len -gt $script:pos) {
      $fs = [IO.File]::Open($log, 'Open', 'Read', 'ReadWrite')
      $fs.Seek($script:pos, 'Begin') | Out-Null
      $buf = New-Object byte[] ($len - $script:pos)
      $fs.Read($buf, 0, $buf.Length) | Out-Null
      $fs.Close()
      $script:stdout.Write($buf, 0, $buf.Length)
      $script:stdout.Flush()
      $script:pos = $len
    }
  }
}
while ($true) {
  Drain
  if (Test-Path $exitFile) {
    Start-Sleep -Milliseconds 250
    Drain
    $code = (Get-Content -Path $exitFile -Raw).Trim()
    if ($code -match '^-?\d+$') { exit ([int]$code) }
  }
  Start-Sleep -Milliseconds 500
}
`

// extractRunnerZipScript unpacks the pushed runner zip into runnerDirWindows.
// tar's own exit-code propagation through -EncodedCommand is fragile across
// PowerShell versions/modes ($LASTEXITCODE isn't reliably the script's own
// exit code unless asked for explicitly) — a corrupt zip could otherwise
// read as success. The trailing `exit $LASTEXITCODE` makes tar's own result
// the script's result, explicitly.
func extractRunnerZipScript(runnerTarball string) string {
	return fmt.Sprintf(`if (!(Test-Path '%s')) { New-Item -ItemType Directory -Path '%s' | Out-Null }
tar -xf '%s\%s' -C '%s'
exit $LASTEXITCODE`, runnerDirWindows, runnerDirWindows, runnerCacheDirWindows, runnerTarball, runnerDirWindows)
}

// commitJITConfigScript renames the scp-delivered .tmp blob into the
// launcher's watched path. The rename is a separate exec from the scp
// stream (scp can only write, not rename), which is exactly what preserves
// the write-then-rename atomicity: the launcher only ever observes
// jitPathWindows, written by a single same-volume Move-Item, never
// partially. -ErrorAction Stop is load-bearing: Move-Item's own errors are
// non-terminating by default, so without it a failed rename would still
// exit 0.
func commitJITConfigScript() string {
	return fmt.Sprintf(`Move-Item -Force -Path '%s' -Destination '%s' -ErrorAction Stop`, jitPendingPathWindows, jitPathWindows)
}

// startRunnerWindows hands the runner off to the image's launcher and
// starts the watcher session that becomes the returned Proc: extract the
// pushed zip, deliver the JIT config over stdin (never the command string —
// same secrecy rule StartRunner's doc comment states for the POSIX path),
// commit it with an atomic rename, then start watcherScriptWindows.
func (g *Guest) startRunnerWindows(ctx context.Context, jit, runnerTarball string) (statemachine.Proc, error) {
	if !runnerZipRE.MatchString(runnerTarball) {
		return nil, fmt.Errorf("refusing to stage runner zip with an unexpected name %q", runnerTarball)
	}
	prep, cancel := bounded.WithTimeout(ctx, windowsPrepTimeout)
	defer cancel()

	out, code, err := g.c.Output(prep, encodedCommand(extractRunnerZipScript(runnerTarball)))
	if err != nil {
		return nil, fmt.Errorf("extracting runner zip: %w", err)
	}
	if code != 0 {
		return nil, fmt.Errorf("extracting runner zip: exit %d: %s", code, out)
	}

	// The header name is advisory (the sink target is a full file path, not a
	// directory); a literal avoids filepath.Base's host-OS separator rules.
	out, code, err = g.c.RunWithInput(prep, scpSinkCommand(jitPendingPathWindows),
		scpSource(".jitconfig.tmp", int64(len(jit)), strings.NewReader(jit)))
	if err != nil {
		return nil, fmt.Errorf("delivering JIT config: %w", err)
	}
	if code != 0 {
		return nil, fmt.Errorf("delivering JIT config: exit %d: %s", code, out)
	}

	out, code, err = g.c.Output(prep, encodedCommand(commitJITConfigScript()))
	if err != nil {
		return nil, fmt.Errorf("committing JIT config: %w", err)
	}
	if code != 0 {
		return nil, fmt.Errorf("committing JIT config: exit %d: %s", code, out)
	}

	p, err := g.c.Start(ctx, encodedCommand(watcherScriptWindows), nil)
	if err != nil {
		return nil, fmt.Errorf("starting runner watcher: %w", err)
	}
	return proc{p}, nil
}

const runnerTarballPlaceholder = "__RUNNER_TARBALL__"

// runStartMarker is the line that launches the runner. It is the anchor guest
// env `export`s and guest_setup commands are injected before, so run.sh
// inherits them; pinned by TestProvisionScriptsPinRunMarker so a refactor
// can't silently move it.
const runStartMarker = "exec ./run.sh"

// guestEnvExports renders a pool's guest_env as shell `export` lines to prepend
// to the runner launch, so run.sh and every job step it spawns inherit them.
// Keys are emitted sorted (deterministic script bytes; they are already
// validated as env-var names at config load). Values are POSIX single-quote
// escaped — wrapped in '...' with each embedded ' rewritten as '\” — so any
// value (quotes, spaces, $) is inert in the shell. Empty input renders nothing,
// keeping provisioning byte-identical for a pool without guest_env.
func guestEnvExports(env map[string]string) string {
	if len(env) == 0 {
		return ""
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		esc := strings.ReplaceAll(env[k], "'", `'\''`)
		fmt.Fprintf(&b, "export %s='%s'\n", k, esc)
	}
	return b.String()
}

// guestSetupBlock renders a pool's guest_setup as newline-joined shell
// commands to run after the guest_env exports and before the runner launches.
// Entries are injected verbatim — they are commands, not identifiers, so
// (unlike guest_env keys) their content can't be validated beyond the
// non-empty check already done at config load. Empty input renders nothing,
// keeping provisioning byte-identical for a pool without guest_setup.
func guestSetupBlock(cmds []string) string {
	if len(cmds) == 0 {
		return ""
	}
	var b strings.Builder
	for _, cmd := range cmds {
		b.WriteString(cmd)
		b.WriteString("\n")
	}
	return b.String()
}

// runnerTarballRE constrains the POSIX tarball basename the daemon
// substitutes into the provision script. The name is daemon-resolved
// (GitHub's asset filename), not client input, but it crosses into a shell
// command string, so this is a trust-boundary guard: the charset carries no
// shell metacharacter and no `/`, so the validated name is inert inside the
// script's double-quoted "$CACHE/…".
var runnerTarballRE = regexp.MustCompile(`^[A-Za-z0-9._-]+\.tar\.gz$`)

// runnerZipRE is runnerTarballRE's windows counterpart: same charset
// rationale (the name crosses into a PowerShell command string), matching
// GitHub's actions-runner-win-<arch>-<ver>.zip asset shape instead of
// .tar.gz.
var runnerZipRE = regexp.MustCompile(`^[A-Za-z0-9._-]+\.zip$`)

// runnerAssetRE picks the trust-boundary guard for the guest's runner-asset
// basename: the tarball pattern for darwin/linux, the zip pattern for
// windows.
func runnerAssetRE(goos string) *regexp.Regexp {
	if goos == home.OSWindows {
		return runnerZipRE
	}
	return runnerTarballRE
}

// provisionScript renders the POSIX (darwin/linux) provision script for the
// exact tarball this cycle resolved. It refuses a name that does not match
// runnerTarballRE rather than risk staging a glob (silent wrong-version) or
// interpolating an unexpected string into the command — fail the cycle
// loudly instead. Never called for a windows guest: StartRunner dispatches
// windows to startRunnerWindows before reaching here, since the windows
// launch is a launcher hand-off plus a watcher session, not a single
// exec'd script.
func provisionScript(goos, runnerTarball string, env map[string]string, setup []string, needsPush bool) (string, error) {
	if goos == home.OSWindows {
		return "", errors.New("provisionScript: windows guests never take the POSIX provision path (see StartRunner)")
	}
	if !runnerTarballRE.MatchString(runnerTarball) {
		return "", fmt.Errorf("refusing to stage runner tarball with an unexpected name %q", runnerTarball)
	}
	// needsPush is the caller's vm.Machine.NeedsRunnerPush() value (true on
	// windows, see hcs_windows.go's doc comment): the same signal that gated
	// whether PushRunnerTarball ran before this script, so the tarball's
	// actual location and the script that looks for it can never disagree.
	linuxScript := provisionScriptLinux
	if needsPush {
		linuxScript = provisionScriptLinuxPushed
	}
	script, err := perOS(goos, provisionScriptDarwin, linuxScript)
	if err != nil {
		return "", err
	}
	script = strings.ReplaceAll(script, runnerTarballPlaceholder, runnerTarball)
	// Prepend the pool's guest_env exports, then its guest_setup commands, to
	// the runner launch: run.sh and every job step inherit the env, and setup
	// runs with it already in scope. Empty env/setup is a no-op (block == ""),
	// leaving the script byte-identical.
	block := guestEnvExports(env) + guestSetupBlock(setup)
	if block != "" {
		script = strings.Replace(script, runStartMarker, block+runStartMarker, 1)
	}
	return script, nil
}

// pullDiagScriptWindows mirrors the POSIX PullDiag shape (a "==> name <=="
// header per file, each tailed to the same 32KiB bound) over
// C:\actions-runner\_diag instead of $HOME/runny-runner/_diag.
const pullDiagScriptWindows = `Get-ChildItem -Path '` + runnerDirWindows + `\_diag' -Filter *.log -ErrorAction SilentlyContinue | ForEach-Object {
  Write-Output "==> $($_.FullName) <=="
  $bytes = [IO.File]::ReadAllBytes($_.FullName)
  if ($bytes.Length -gt 32768) { $bytes = $bytes[($bytes.Length - 32768)..($bytes.Length - 1)] }
  [Console]::Out.Write([Text.Encoding]::UTF8.GetString($bytes))
  Write-Output ""
}
`

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

// stopRunnerScript kills the runner LISTENER tree and proves it dead. Every
// process in that tree carries "--jitconfig <blob>" in argv; the [-]-bracket
// makes the ERE match "--jitconfig" without matching the pattern's own literal
// text. Exit 0 = proven dead; 1 = survived SIGKILL; 2 = verification tool
// failure. TERM→KILL at 3s, hard bound ~6s — inside secure_ssh (15s).
//
// Scope: the proof targets the listener (the job-eligibility surface).
// Runner.Worker and job-step processes do not reliably carry --jitconfig and
// may survive the pkill; a dead listener plus single-use JIT is the
// no-new-jobs guarantee. pkill/pgrep ship on both guest OSes.
const stopRunnerScript = `PAT='[-]-jitconfig'
alive() {
  pgrep -f "$PAT" >/dev/null 2>&1
  case $? in
    0) return 0 ;;
    1) return 1 ;;
    *) echo "runny: pgrep failed; cannot verify runner death" >&2; exit 2 ;;
  esac
}
pkill -TERM -f "$PAT" 2>/dev/null
i=0
while alive; do
  i=$((i+1))
  [ "$i" -eq 12 ] && pkill -KILL -f "$PAT" 2>/dev/null
  if [ "$i" -gt 24 ]; then echo "runny: runner still alive after SIGKILL" >&2; exit 1; fi
  sleep 0.25
done
`

// debugSessionLogFile is the path on the guest where the recorder writes and
// teardown reads back the operator's session log. A single constant keeps the
// recorder scripts and PullDebugSession in sync.
const debugSessionLogFile = "/tmp/runny-debug-session.log"

// debugRecorderDarwin / debugRecorderLinux are the /tmp/runny-record wrapper
// scripts written alongside an operator debug key. The wrapper forces every
// use of that key (interactive shell, non-interactive command, and direct
// reconnects after runnyctl debug exits) through script(1), appending all
// output to debugSessionLogFile for teardown to pull.
//
// The split mirrors provisionScriptDarwin / provisionScriptLinux: BSD script(1)
// uses a positional command form; util-linux uses -c. The fallback ensures an
// operator is never locked out when script is absent — record nothing rather
// than deny access.
const debugRecorderDarwin = "#!/bin/sh\n" +
	"if ! command -v script >/dev/null 2>&1; then\n" +
	"  if [ -n \"$SSH_ORIGINAL_COMMAND\" ]; then exec \"${SHELL:-/bin/sh}\" -c \"$SSH_ORIGINAL_COMMAND\"; fi\n" +
	"  exec \"${SHELL:-/bin/sh}\"\n" +
	"fi\n" +
	"if [ -n \"$SSH_ORIGINAL_COMMAND\" ]; then\n" +
	"  exec script -q -F -a " + debugSessionLogFile + " /bin/sh -c \"$SSH_ORIGINAL_COMMAND\"\n" +
	"else\n" +
	"  exec script -q -F -a " + debugSessionLogFile + "\n" +
	"fi\n"

const debugRecorderLinux = "#!/bin/sh\n" +
	"if ! command -v script >/dev/null 2>&1; then\n" +
	"  if [ -n \"$SSH_ORIGINAL_COMMAND\" ]; then exec \"${SHELL:-/bin/sh}\" -c \"$SSH_ORIGINAL_COMMAND\"; fi\n" +
	"  exec \"${SHELL:-/bin/sh}\"\n" +
	"fi\n" +
	"if [ -n \"$SSH_ORIGINAL_COMMAND\" ]; then\n" +
	"  exec script -q -f -a -c \"$SSH_ORIGINAL_COMMAND\" -e " + debugSessionLogFile + "\n" +
	"else\n" +
	"  exec script -q -f -a " + debugSessionLogFile + "\n" +
	"fi\n"

// installDebugKeyScript writes the per-OS session recorder to /tmp/runny-record,
// then appends a command=-wrapped authorized_keys line and greps back the full
// wrapped line to prove the command= wrapper landed (not just the bare key).
// The command= option forces every operator SSH session through the recorder
// regardless of what the client requests. restrict denies forwarding/X11/agent;
// pty re-grants the PTY restrict would otherwise deny (which script(1) needs).
// The daemon's own cycle key is a separate, unwrapped line, so daemon
// operations are unaffected.
//
// Format args: recorder-script-content, key-line, key-line.
const installDebugKeyScript = `set -e
umask 077
mkdir -p "$HOME/.ssh"
printf '%%s' '%s' > /tmp/runny-record
chmod 0755 /tmp/runny-record
printf '%%s\n' 'command="exec /tmp/runny-record",restrict,pty %s' >> "$HOME/.ssh/authorized_keys"
grep -qF -- 'command="exec /tmp/runny-record",restrict,pty %s' "$HOME/.ssh/authorized_keys"
`

// stopRunnerScriptWindows is the windows equivalent of stopRunnerScript:
// find the listener tree by its --jitconfig argv (Get-CimInstance
// Win32_Process exposes CommandLine, pgrep/pkill's role here), kill it, and
// prove it dead by re-checking. Excluding this powershell process's own
// PID/parent PID is defense in depth mirroring the POSIX [-]-jitconfig
// trick, though the risk it guards against doesn't actually exist here:
// this script's own commandline is `powershell -EncodedCommand <base64>`,
// which never contains the literal text "--jitconfig" to self-match against.
// Exit 0 = proven dead; 1 = survived Stop-Process.
const stopRunnerScriptWindows = `$pat = '--jitconfig'
$self = $PID
function Alive {
  Get-CimInstance Win32_Process | Where-Object { $_.CommandLine -like "*$pat*" -and $_.ProcessId -ne $self -and $_.ParentProcessId -ne $self }
}
Alive | ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
$i = 0
while (Alive) {
  $i++
  if ($i -gt 24) { Write-Error 'runny: runner still alive after Stop-Process'; exit 1 }
  Start-Sleep -Milliseconds 250
}
exit 0
`

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

// installDebugKeyScriptWindows appends the operator's key to
// administrators_authorized_keys and fixes its ACL — psAppendAuthorizedKeyLine,
// the same fragment rotateScriptWindowsTemplate uses, so the ACL fix has
// exactly one home — then proves the line landed via a read-back. No
// command= recording wrapper: see PullDebugSession's doc comment for why
// Windows debug sessions aren't recorded.
var installDebugKeyScriptWindows = `$line = '%s'
` + psAppendAuthorizedKeyLine("$line") + `if (-not (Select-String -Path '` + administratorsAuthorizedKeysPath + `' -SimpleMatch '%s' -Quiet)) { exit 1 }
`

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
