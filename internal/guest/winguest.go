// This file holds the windows dialect: the -EncodedCommand and scp-sink
// byte-channel helpers, the rotate/scramble/stop/diag/watcher/debug-key PS
// scripts, and startRunnerWindows/rotateWindows. It builds on every host —
// the FSM tests exercise the windows dialect on macOS/Linux CI — so it is
// deliberately NOT named guest_windows.go, which Go's implicit GOOS file
// suffix would constrain to windows-only hosts. A declaration belongs here
// iff it is used only by the windows side of a dispatcher method in
// guest.go — see internal/guest/CLAUDE.md for the split's rationale.
package guest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"math/big"
	"regexp"
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
// The restart runs INLINE, directly — not detached. A detached Start-Process
// was tried first on the theory that restarting sshd from inside the
// session issuing the restart would kill that very session (Windows sshd
// connections are children of the service process, unlike systemd's
// per-connection reload on Linux). Hardware-proven wrong against the real
// image: the detached child never actually reached Restart-Service (the
// service's PID never changed), while a direct inline Restart-Service
// produced a new PID immediately and left the issuing session alive to
// report its own exit status. It also wouldn't matter if some other image's
// sshd DOES drop the session here — rotateWindows discards this connection
// either way and reconnects fresh with the new key (sshx.WaitFor below), so
// there is nothing about this session's survival worth protecting.
var rotateScriptWindowsTemplate = `$pub = '%s'
` + psAppendAuthorizedKeyLine("$pub") + `$cfgPath = 'C:\ProgramData\ssh\sshd_config'
$existing = Get-Content -Path $cfgPath -Raw
Set-Content -Path $cfgPath -Value ("PasswordAuthentication no` + "`r`n" + `" + $existing) -NoNewline
%sRestart-Service sshd -ErrorAction Stop
`

// scrambleLineWindowsTemplate randomizes the AUTHENTICATED account's password
// (ssh_hardening: scramble). $env:USERNAME resolves the account the sshd
// session runs as — the Windows equivalent of the POSIX path's $(id -un) —
// rather than a hardcoded name: hardcoding "Administrator" would scramble the
// wrong account whenever the pool's ssh_user is any other administrator, so
// the real account would keep its well-known password while verification
// (against the never-installed generated password) still saw a rejection and
// falsely passed. -ErrorAction Stop is load-bearing: Set-LocalUser's errors
// are non-terminating by default (e.g. a generated password that violates
// guest policy), so without it a failed scramble would sail through to the
// restart and return 0, leaving the well-known password live while
// rotation reports success — the same defensive posture Move-Item and icacls
// already take in this file. Set-LocalUser, not `+"`net user`"+`: net user
// prompts interactively for any password over 14 characters instead of
// accepting one on the command line, which would hang the exec forever.
const scrambleLineWindowsTemplate = `Set-LocalUser -Name $env:USERNAME -Password (ConvertTo-SecureString '%s' -AsPlainText -Force) -ErrorAction Stop
`

// windowsPasswordClasses are the four character classes Windows' default
// password complexity policy scores against (it requires at least 3 of 4
// present). crypto/rand.Text's alphabet (uppercase + digits 2-7 only) can
// satisfy at most 2, so Set-LocalUser rejects every password it produces
// with InvalidPasswordException on any guest enforcing that policy —
// hardware-proven against the real image, not theoretical.
// windowsScramblePassword guarantees at least one character from every
// class instead of merely a likely majority.
var windowsPasswordClasses = [...]string{
	"ABCDEFGHIJKLMNOPQRSTUVWXYZ",
	"abcdefghijklmnopqrstuvwxyz",
	"0123456789",
	"!@#$%^&*()-_=+",
}

// windowsScramblePasswordLength is arbitrary but ample: Set-LocalUser accepts
// up to 127 characters, well past what's needed here once one character from
// every class above is already guaranteed.
const windowsScramblePasswordLength = 24

// windowsScramblePassword generates the Windows account scramble password:
// one character from every windowsPasswordClasses entry, then the rest drawn
// from their union, Fisher-Yates shuffled so the guaranteed picks aren't in
// fixed positions.
func windowsScramblePassword() string {
	all := strings.Join(windowsPasswordClasses[:], "")
	pw := make([]byte, windowsScramblePasswordLength)
	for i, class := range windowsPasswordClasses {
		pw[i] = class[randIndex(len(class))]
	}
	for i := len(windowsPasswordClasses); i < len(pw); i++ {
		pw[i] = all[randIndex(len(all))]
	}
	for i := len(pw) - 1; i > 0; i-- {
		j := randIndex(i + 1)
		pw[i], pw[j] = pw[j], pw[i]
	}
	return string(pw)
}

// randIndex returns a cryptographically random int in [0, n). crypto/rand.Reader
// failing means a broken host, not a recoverable condition — same contract
// rand.Text itself panics under.
func randIndex(n int) int {
	i, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		panic(err)
	}
	return int(i.Int64())
}

// rotateWindows is Rotate's windows branch: same mint → capture → install →
// reconnect → prove-the-negative choreography as the POSIX path, but every
// step is a PowerShell script against Windows OpenSSH's own config surface
// (administrators_authorized_keys, sshd_config, the sshd service) instead of
// a POSIX one-liner. See rotateScriptWindowsTemplate's doc comment for the
// ACL, prepend, and inline-restart traps.
func (d Dialer) rotateWindows(ctx bounded.Context, addr string, pg *Guest, signer ssh.Signer) (statemachine.Guest, error) {
	out, err := pg.runStepOutput(ctx, "rotate: capturing host keys", encodedCommand(captureHostKeysWindowsScript), nil)
	if err != nil {
		return nil, err
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
		pw := windowsScramblePassword()
		verifyPW = pw
		scramble = fmt.Sprintf(scrambleLineWindowsTemplate, psQuote(pw))
	}
	script := fmt.Sprintf(rotateScriptWindowsTemplate, pubEsc, scramble)

	if err := pg.runStep(ctx, "rotate: installing cycle key", encodedCommand(script), nil); err != nil {
		return nil, err
	}

	// Prove password auth is dead BEFORE dialing the supervision client.
	// Password auth is off only on the post-restart sshd, so
	// verifyPasswordAuthDead — which retries until it sees a rejection —
	// both proves the flip and gates on the restart having landed (the
	// restart's own script exit isn't proof by itself: this session's exit
	// status can't tell us whether a NEW inbound connection sees the old or
	// new sshd). Dialing the client first could bind it to the pre-restart
	// sshd (the cycle key is already installed there too), reporting success
	// against a config that hasn't actually taken effect yet.
	verifyCfg := d.SSH
	verifyCfg.Password = verifyPW
	if err := verifyPasswordAuthDead(ctx, addr, verifyCfg); err != nil {
		return nil, err
	}

	cfg := d.SSH
	cfg.Signer = signer
	cfg.HostKeys = hostKeys
	c, err := sshx.WaitFor(ctx, addr, cfg, d.interval())
	if err != nil {
		return nil, fmt.Errorf("rotate: reconnecting with cycle key: %w", err)
	}

	_ = pg.c.Close()
	return &Guest{c: c, addr: addr, cfg: cfg, interval: d.interval(), goos: home.OSWindows}, nil
}

// runnerCacheDirWindows is where PushRunnerTarball stages the runner zip on a
// windows GUEST, and where StartRunner's extract step reads it from — always
// pushed (a windows guest only ever boots on the HCS host, whose
// NeedsRunnerPush is unconditionally true), so there is no live-share
// variant to keep in sync with, unlike the linux push/mount split above.
const runnerCacheDirWindows = `C:\runny-cache`

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
// Drain decodes the log with the encoding named by its byte-order mark and
// re-emits UTF-8 to the standard-output stream, because the FSM matches the
// "Listening for Jobs" marker as a UTF-8 string. This is load-bearing on
// Windows, not defensive dressing: the image's launcher writes runner.log
// via PowerShell 5.1's `*>` redirect, which encodes the file as UTF-16LE
// (Windows' native "Unicode") — so the raw bytes are `L\0i\0s\0t...`, and a
// UTF-8 substring match never fires. The decode is keyed off the BOM rather
// than assuming any single encoding, because UTF-16LE is the platform
// default any number of Windows tools reach for, and the daemon is the one
// place that has to be right regardless of which one wrote the file. For a
// two-byte encoding the drain window is aligned to a whole number of code
// units ($avail - $avail % 2) so a poll boundary can't split a UTF-16 unit;
// a lone surrogate at a boundary (astral chars, effectively absent from
// runner logs) degrades to U+FFFD, the same acceptable loss the old raw path
// carried for split UTF-8 sequences.
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
$enc = $null
$utf8 = New-Object Text.UTF8Encoding($false)
$stdout = [Console]::OpenStandardOutput()
function ReadFully($fs, $buf, $count) {
  $off = 0
  while ($off -lt $count) {
    $n = $fs.Read($buf, $off, $count - $off)
    if ($n -le 0) { break }
    $off += $n
  }
  return $off
}
function Detect {
  $fs = [IO.File]::Open($log, 'Open', 'Read', 'ReadWrite')
  $h = New-Object byte[] ([Math]::Min(3, $fs.Length))
  ReadFully $fs $h $h.Length | Out-Null
  $fs.Close()
  if ($h.Length -ge 2 -and $h[0] -eq 0xFF -and $h[1] -eq 0xFE) { $script:enc = [Text.Encoding]::Unicode; $script:pos = 2 }
  elseif ($h.Length -ge 2 -and $h[0] -eq 0xFE -and $h[1] -eq 0xFF) { $script:enc = [Text.Encoding]::BigEndianUnicode; $script:pos = 2 }
  elseif ($h.Length -ge 3 -and $h[0] -eq 0xEF -and $h[1] -eq 0xBB -and $h[2] -eq 0xBF) { $script:enc = $script:utf8; $script:pos = 3 }
  else { $script:enc = $script:utf8; $script:pos = 0 }
}
function Drain {
  if (-not (Test-Path $log)) { return }
  if ($null -eq $script:enc) {
    if ((Get-Item $log).Length -lt 2) { return }
    Detect
  }
  $len = (Get-Item $log).Length
  if ($len -le $script:pos) { return }
  $avail = $len - $script:pos
  if ($script:enc.CodePage -eq 1200 -or $script:enc.CodePage -eq 1201) { $avail -= ($avail % 2) }
  if ($avail -le 0) { return }
  $fs = [IO.File]::Open($log, 'Open', 'Read', 'ReadWrite')
  $fs.Seek($script:pos, 'Begin') | Out-Null
  $buf = New-Object byte[] $avail
  $r = ReadFully $fs $buf $avail
  $fs.Close()
  if ($r -le 0) { return }
  $b = $script:utf8.GetBytes($script:enc.GetString($buf, 0, $r))
  $script:stdout.Write($b, 0, $b.Length)
  $script:stdout.Flush()
  $script:pos += $r
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

	if err := g.runStep(prep, "extracting runner zip", encodedCommand(extractRunnerZipScript(runnerTarball)), nil); err != nil {
		return nil, err
	}

	// The header name is advisory (the sink target is a full file path, not a
	// directory); a literal avoids filepath.Base's host-OS separator rules.
	jitInput := scpSource(".jitconfig.tmp", int64(len(jit)), strings.NewReader(jit))
	if err := g.runStep(prep, "delivering JIT config", scpSinkCommand(jitPendingPathWindows), jitInput); err != nil {
		return nil, err
	}

	if err := g.runStep(prep, "committing JIT config", encodedCommand(commitJITConfigScript()), nil); err != nil {
		return nil, err
	}

	p, err := g.c.Start(ctx, encodedCommand(watcherScriptWindows), nil)
	if err != nil {
		return nil, fmt.Errorf("starting runner watcher: %w", err)
	}
	return proc{p}, nil
}

// runnerZipRE is runnerTarballRE's windows counterpart: same charset
// rationale (the name crosses into a PowerShell command string), matching
// GitHub's actions-runner-win-<arch>-<ver>.zip asset shape instead of
// .tar.gz.
var runnerZipRE = regexp.MustCompile(`^[A-Za-z0-9._-]+\.zip$`)

// pullDiagScriptWindows mirrors the POSIX PullDiag shape (a "==> name <=="
// header per file, each read whole) over C:\actions-runner\_diag instead of
// $HOME/runny-runner/_diag.
//
// Each log is opened with explicit ReadWrite sharing, the same reason
// pullDebugSessionScriptWindows and watcherScriptWindows open with it.
// Teardown pulls the post-mortem BEFORE StopRunner, so on exactly the failure
// cycles diag exists for, the listener still holds _diag\Runner_*.log open.
// [IO.File]::ReadAllBytes opens FileShare.Read, which collides with any open
// writer — measured on a Windows host, including against a writer that
// declared FileShare.ReadWrite, because sharing is mutual: a FileShare.Read
// reader refuses to coexist with the writer's Write access no matter how
// permissive that writer was. Only a FileShare.None writer defeats the
// ReadWrite open, so it strictly dominates.
//
// Content streams via Stream.CopyTo rather than being read into a buffer
// first: the logs are pulled whole now, and a byte array would size an
// allocation in the guest to whatever the job happened to log (and cap it at
// Int32 besides). The header is emitted before the copy starts, so a file that
// fails midway leaves what it managed plus the unreadable line.
//
// The path is hoisted into $p before the try because PowerShell rebinds $_ to
// the ErrorRecord inside a catch, shadowing ForEach-Object's pipeline item —
// so $_.FullName there expands to nothing and the unreadable line would name
// no file, which is the one thing it exists to do.
//
// Each file is read inside try/catch to keep a failure attributable. An
// uncaught .NET exception here does not abort the loop — PowerShell reports
// it as non-terminating and ForEach-Object continues to the next file — but
// Output collects stdout and stderr into one buffer, so the exception text
// lands in the middle of the artifact at whatever offset it interleaved at,
// detached from the file it refers to. Catching it turns that into a labelled
// line under the right header, and keeps the pull's own exit code meaning
// what it says.
//
// Output goes through the raw stdout stream rather than [Console]::Out, whose
// TextWriter re-encodes to the console's OEM codepage and would turn any
// non-ASCII in a diag log into "?" — the same reason the two sibling scripts
// avoid it.
const pullDiagScriptWindows = `$stdout = [Console]::OpenStandardOutput()
function Emit($s) { $b = [Text.Encoding]::UTF8.GetBytes($s); $stdout.Write($b, 0, $b.Length) }
Get-ChildItem -Path '` + runnerDirWindows + `\_diag' -Filter *.log -ErrorAction SilentlyContinue | ForEach-Object {
  $p = $_.FullName
  try {
    $fs = [IO.File]::Open($p, 'Open', 'Read', 'ReadWrite')
    try {
      Emit "==> $p <==` + "`n" + `"
      $fs.CopyTo($stdout)
      Emit "` + "`n" + `"
    } finally { $fs.Close() }
  } catch {
    Emit "==> $p <== (unreadable: $($_.Exception.Message))` + "`n" + `"
  }
}
$stdout.Flush()
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
  try {
    Get-CimInstance Win32_Process -ErrorAction Stop | Where-Object { $_.CommandLine -like "*$pat*" -and $_.ProcessId -ne $self -and $_.ParentProcessId -ne $self }
  } catch {
    Write-Error "runny: CIM process query failed; cannot verify runner death: $_"
    exit 2
  }
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

// debugSessionLogFileWindows is where the recorder writes and
// PullDebugSession reads back the operator's windows session log — the
// windows counterpart of debugSessionLogFile.
const debugSessionLogFileWindows = `C:\ProgramData\ssh\runny-debug-session.log`

// debugRecorderScriptPathWindows is where installDebugKeyScriptWindows writes
// debugRecorderScriptWindows — the windows counterpart of POSIX's
// /tmp/runny-record path.
const debugRecorderScriptPathWindows = `C:\ProgramData\ssh\runny-record.ps1`

// debugRecorderScriptWindows is forced on every use of the operator's debug
// key via authorized_keys' command= option — the windows counterpart of
// debugRecorderDarwin/Linux's /tmp/runny-record. It is a static, argument-free
// script with no embedded quoting hazard, so the forced command that invokes
// it is a plain -File call, not encodedCommand: encodedCommand exists for
// scripts the daemon composes at runtime with templated or multi-line content
// over a live SSH session (internal/guest/CLAUDE.md); this script is neither.
//
// Unlike POSIX, no single windows mechanism records both SSH usage shapes —
// hardware-proven against a real Windows OpenSSH host (issue #344):
//
//   - SSH_ORIGINAL_COMMAND set (a one-shot `ssh host "cmd"` exec): the
//     command runs as a cmd.exe child, its combined output piped through
//     Tee-Object. Tee-Object operates on the pipeline's own object stream,
//     not console rendering, so it captures a native process's output
//     whether or not a pty is attached — proven where Start-Transcript is
//     proven NOT to: Start-Transcript silently drops a child process's
//     console output in this exact shape, no error, nothing.
//   - SSH_ORIGINAL_COMMAND unset (an interactive `ssh -t host` shell): a
//     nested interactive powershell -NoExit is spawned UNPIPED — piping would
//     sever the operator's own stdin from it, losing the prompt and
//     everything they type — with Start-Transcript already running inside
//     it. Start-Transcript hooks .NET Console.Out, which typed commands and
//     PowerShell-native cmdlet output flow through, and is proven to capture
//     the prompt, input, and cmdlet output faithfully. It does NOT capture a
//     native/external program's output in this branch (WriteConsole bypasses
//     the Console.Out hook entirely) — a Windows PowerShell 5.1 ConsoleHost
//     limitation proven on the real thing, with no workaround short of
//     owning the ConPTY layer, which sshd does, not this script. An operator
//     who needs guaranteed capture of a build tool's output should run it
//     non-interactively instead.
const debugRecorderScriptWindows = `$log = '` + debugSessionLogFileWindows + `'
if ($env:SSH_ORIGINAL_COMMAND) {
  & $env:ComSpec /c $env:SSH_ORIGINAL_COMMAND 2>&1 | Tee-Object -FilePath $log -Append
} else {
  & powershell.exe -NoProfile -NoExit -Command "Start-Transcript -Path '` + debugSessionLogFileWindows + `' -Append | Out-Null"
}
exit $LASTEXITCODE
`

// installDebugKeyScriptWindows writes debugRecorderScriptWindows to
// debugRecorderScriptPathWindows (-ErrorAction Stop: Set-Content's own errors
// are non-terminating by default, so without it a failed write would still
// fall through to a "successful" readback that only proves the
// authorized_keys line landed, not that the recorder file exists), then
// appends a command=-wrapped authorized_keys line via
// psAppendAuthorizedKeyLine (the one ACL-fix home, shared with rotate) and
// reads back the FULL wrapped line — not just the bare key — to prove the
// command= wrapper itself landed, the same guarantee installDebugKeyScript's
// grep already proves on the POSIX side. restrict,pty mirrors the POSIX
// line's own options: restrict denies forwarding/agent/X11, pty re-grants
// the PTY restrict would otherwise deny.
//
// debugRecorderScriptWindows is passed as a %s Sprintf ARGUMENT, not spliced
// into this format string via +, the same discipline installDebugKeyScript
// uses for its own recorder content: Sprintf treats any literal % in the
// FORMAT STRING as a verb, so splicing free-form script content directly in
// would be a latent corruption landmine the moment that content gained one
// (e.g. a %TEMP%-style reference) — passing it as an argument is immune by
// construction, regardless of its content.
//
// Format args: the canonicalized "type base64" key line, debugRecorderScriptWindows.
var installDebugKeyScriptWindows = `$line = 'command="powershell -NoProfile -File ` + debugRecorderScriptPathWindows + `",restrict,pty %s'
Set-Content -Path '` + debugRecorderScriptPathWindows + `' -Value @'
%s
'@ -ErrorAction Stop
` + psAppendAuthorizedKeyLine("$line") + `if (-not (Select-String -Path '` + administratorsAuthorizedKeysPath + `' -SimpleMatch $line -Quiet)) { exit 1 }
`

// pullDebugSessionScriptWindows reads debugSessionLogFileWindows back for
// PullDebugSession. It cannot assume UTF-8 like pullDiagScriptWindows does:
// Tee-Object/Out-File's default encoding on Windows PowerShell 5.1 is
// UTF-16LE with a BOM — measured directly off a real recorded session, not
// assumed — so the read goes through StreamReader's own BOM detection
// (detectEncodingFromByteOrderMarks: true falls back to the passed UTF8
// default when no BOM is present) rather than a hand-rolled sniff, and
// re-emits UTF-8 over the raw stdout stream (never the OEM-codepage
// [Console]::Out TextWriter, the same reason watcherScriptWindows avoids
// it). The file is opened with explicit ReadWrite sharing, the same reason
// watcherScriptWindows's own Detect/ReadFully open with it: this read can
// race a still-live operator session (recycle -force or hold-expiry while
// the operator is connected is a primary use case, not an edge case), and a
// default-shared read would throw on a sharing violation against
// Start-Transcript's still-open handle instead of returning whatever was
// captured so far. No session this cycle (the log never existing) is
// silent success, mirroring PullDebugSession's POSIX `cat ... || true`.
const pullDebugSessionScriptWindows = `$log = '` + debugSessionLogFileWindows + `'
if (-not (Test-Path $log)) { exit 0 }
$fs = [IO.File]::Open($log, 'Open', 'Read', 'ReadWrite')
$sr = New-Object IO.StreamReader($fs, [Text.Encoding]::UTF8, $true)
$out = [Text.Encoding]::UTF8.GetBytes($sr.ReadToEnd())
$sr.Close()
$stdout = [Console]::OpenStandardOutput()
$stdout.Write($out, 0, $out.Length)
$stdout.Flush()
`
