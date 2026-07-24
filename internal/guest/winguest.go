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

// runnerZipRE is runnerTarballRE's windows counterpart: same charset
// rationale (the name crosses into a PowerShell command string), matching
// GitHub's actions-runner-win-<arch>-<ver>.zip asset shape instead of
// .tar.gz.
var runnerZipRE = regexp.MustCompile(`^[A-Za-z0-9._-]+\.zip$`)

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

// installDebugKeyScriptWindows appends the operator's key to
// administrators_authorized_keys and fixes its ACL — psAppendAuthorizedKeyLine,
// the same fragment rotateScriptWindowsTemplate uses, so the ACL fix has
// exactly one home — then proves the line landed via a read-back. No
// command= recording wrapper: see PullDebugSession's doc comment for why
// Windows debug sessions aren't recorded.
var installDebugKeyScriptWindows = `$line = '%s'
` + psAppendAuthorizedKeyLine("$line") + `if (-not (Select-String -Path '` + administratorsAuthorizedKeysPath + `' -SimpleMatch '%s' -Quiet)) { exit 1 }
`
