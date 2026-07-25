// Tests for the windows dialect (winguest.go) that don't need the shared
// rotateServer/testDialer SSH test harness — pure unit tests over the PS
// script builders and byte-transfer helpers. Named to dodge Go's implicit
// _windows GOOS suffix, same as winguest.go itself, so these run on every
// host.
package guest

import (
	"encoding/base64"
	"fmt"
	"io"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/bojanrajkovic/runny/internal/home"
)

// The windows rotate script must set the administrators_authorized_keys ACL
// (sshd silently ignores the file otherwise), PREPEND PasswordAuthentication
// no (sshd_config is first-match-wins, and the stock config ends with a
// Match Group administrators block an append would land inside), and
// restart sshd INLINE, directly, not through a detached Start-Process.
//
// A detached restart was tried first on the theory that an inline restart
// would kill the session issuing it (Windows sshd connections are children
// of the service process) — hardware-proven wrong against the real image:
// the detached Start-Process child never actually reached Restart-Service
// (same service PID before and after, twice), while a direct inline
// Restart-Service produced a new PID immediately and left the issuing
// session alive to report its own exit status. It also doesn't matter if a
// future image's sshd DOES drop the session here: rotateWindows discards
// this connection either way and reconnects fresh with the new key
// (sshx.WaitFor), so there's nothing on this session's survival to protect.
func TestRotateScriptWindowsPinsACLPrependAndInlineRestart(t *testing.T) {
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
	if !strings.Contains(script, "\nRestart-Service sshd -ErrorAction Stop") {
		t.Errorf("windows rotate script must restart sshd directly, inline, aborting loudly on failure:\n%s", script)
	}
	if strings.Contains(script, "Start-Process") {
		t.Errorf("windows rotate script must not detach the restart via Start-Process:\n%s", script)
	}
}

// Plain rotate must never touch the account password; scramble uses
// Set-LocalUser (net user prompts interactively above 14 chars and would
// hang the exec), targets the AUTHENTICATED account ($env:USERNAME, not a
// hardcoded name — scrambling the wrong account leaves the real one's
// well-known password live), and aborts loudly on failure (-ErrorAction Stop,
// since Set-LocalUser errors are non-terminating by default).
func TestRotateScrambleWindowsUsesSetLocalUser(t *testing.T) {
	if strings.Contains(fmt.Sprintf(scrambleLineWindowsTemplate, "x"), "net user") {
		t.Error("windows scramble line must not use net user")
	}
	if !strings.Contains(scrambleLineWindowsTemplate, "Set-LocalUser") {
		t.Error("windows scramble line must use Set-LocalUser")
	}
	if !strings.Contains(scrambleLineWindowsTemplate, "-Name $env:USERNAME") {
		t.Error("windows scramble line must target the authenticated account via $env:USERNAME, not a hardcoded name")
	}
	if !strings.Contains(scrambleLineWindowsTemplate, "-ErrorAction Stop") {
		t.Error("windows scramble line must abort on a failed Set-LocalUser (-ErrorAction Stop)")
	}
}

// crypto/rand.Text's alphabet (uppercase + digits 2-7 only) satisfies at most 2
// of the 4 classes Windows' default password complexity policy scores against
// (it requires >=3 of 4), so Set-LocalUser rejects every password it produces
// with InvalidPasswordException on a guest enforcing that policy — hardware-
// proven against the real image, not theoretical. windowsScramblePassword must
// GUARANTEE all four classes present, not just probably include them.
func TestWindowsScramblePasswordSatisfiesComplexity(t *testing.T) {
	classes := []struct {
		name string
		has  func(byte) bool
	}{
		{"upper", func(b byte) bool { return b >= 'A' && b <= 'Z' }},
		{"lower", func(b byte) bool { return b >= 'a' && b <= 'z' }},
		{"digit", func(b byte) bool { return b >= '0' && b <= '9' }},
		{"special", func(b byte) bool { return strings.ContainsRune("!@#$%^&*()-_=+", rune(b)) }},
	}
	for i := 0; i < 100; i++ {
		pw := windowsScramblePassword()
		if len(pw) < 12 {
			t.Fatalf("password too short for a real complexity margin: %q (%d chars)", pw, len(pw))
		}
		if strings.ContainsRune(pw, '\'') {
			t.Fatalf("password must not contain a single quote (breaks the PS single-quoted literal): %q", pw)
		}
		for _, c := range classes {
			found := false
			for j := 0; j < len(pw); j++ {
				if c.has(pw[j]) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("iteration %d: password %q missing required class %q", i, pw, c.name)
			}
		}
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
	// The verification-tool-failure arm of the 0/1/2 contract: a failed CIM
	// query must exit 2 ("cannot verify"), never fall through to the empty
	// result reading as proven-dead — the same arm the POSIX script implements
	// for a pgrep failure.
	if !strings.Contains(stopRunnerScriptWindows, "exit 2") || !strings.Contains(stopRunnerScriptWindows, "-ErrorAction Stop") {
		t.Error("windows stop script must exit 2 when the CIM process query itself fails")
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

// The JIT blob is scp-streamed to the .tmp path and committed by a separate
// Move-Item exec with -ErrorAction Stop (Move-Item's own errors are
// non-terminating by default; without it a failed rename silently exits 0).
// The rename staying a single same-volume Move-Item is what keeps the
// launcher from ever reading a half-written blob.
func TestCommitJITConfigScriptRenamesAtomically(t *testing.T) {
	script := commitJITConfigScript()
	if !strings.Contains(script, "Move-Item -Force -Path '"+jitPendingPathWindows+"' -Destination '"+jitPathWindows+"' -ErrorAction Stop") {
		t.Errorf("script must rename into place with -ErrorAction Stop:\n%s", script)
	}
	if strings.Contains(script, "OpenStandardInput") {
		t.Errorf("JIT commit must not read stdin through the PowerShell host:\n%s", script)
	}
}

// File transfers speak SCP to the guest's native scp.exe sink — never a
// PowerShell stdin read, which the PS 5.1 console-less host wedges or kills
// (hardware-proven on the published image). The framing is one C-record:
// header, bytes, NUL.
func TestScpSourceFraming(t *testing.T) {
	got, err := io.ReadAll(scpSource("x.bin", 3, strings.NewReader("abc")))
	if err != nil {
		t.Fatalf("reading scp source stream: %v", err)
	}
	want := "C0644 3 x.bin\nabc\x00"
	if string(got) != want {
		t.Errorf("scp framing = %q, want %q", got, want)
	}
	if cmd := scpSinkCommand(`C:\runny-cache\x.bin`); cmd != `scp -t C:\runny-cache\x.bin` {
		t.Errorf("scp sink command = %q", cmd)
	}
}

// Every EncodedCommand script runs with the progress stream silenced: PS
// 5.1's console-less host otherwise serializes progress records (module
// autoload's "Preparing modules for first use") as a #< CLIXML blob on
// stderr, polluting parsed output — the rotate's host-key capture hit
// exactly this on the published image.
func TestEncodedCommandSilencesProgressStream(t *testing.T) {
	cmd := encodedCommand("Get-Date")
	b64 := strings.TrimPrefix(cmd, "powershell -NoProfile -NonInteractive -EncodedCommand ")
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("decoding encoded command: %v", err)
	}
	u16 := make([]uint16, len(raw)/2)
	for i := range u16 {
		u16[i] = uint16(raw[2*i]) | uint16(raw[2*i+1])<<8
	}
	script := string(utf16.Decode(u16))
	if !strings.HasPrefix(script, "$ProgressPreference='SilentlyContinue'; ") {
		t.Errorf("encoded script must silence the progress stream first: %q", script)
	}
	if !strings.HasSuffix(script, "Get-Date") {
		t.Errorf("encoded script must end with the caller's script: %q", script)
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
	debug := fmt.Sprintf(installDebugKeyScriptWindows, "AAAA key", debugRecorderScriptWindows)
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
// hand-off relies on. It decodes the log by its BOM and re-emits UTF-8 so the
// FSM's UTF-8 "Listening for Jobs" match fires: the launcher's PowerShell 5.1
// `*>` redirect writes runner.log as UTF-16LE, and a raw-byte forward would
// never match. Output still goes to the raw stdout stream (never the
// OEM-codepage [Console]::Out TextWriter). The exit-code read is guarded by
// an integer regex before the cast.
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
	if !strings.Contains(watcherScriptWindows, `$code -match '^-?\d+$'`) {
		t.Error("watcher script must guard the exit-code cast with an integer regex that accepts negative (crashed-process NTSTATUS) codes")
	}
	if strings.Contains(watcherScriptWindows, "[Console]::Out.Write") {
		t.Error("watcher script must not write through [Console]::Out (OEM codepage re-encoding)")
	}
	if !strings.Contains(watcherScriptWindows, "OpenStandardOutput()") {
		t.Error("watcher script must write to the standard-output stream")
	}
	// The load-bearing fix: detect the log's BOM and decode before re-emitting
	// UTF-8, because the launcher writes runner.log as UTF-16LE. Without this
	// the "Listening for Jobs" match never fires and every windows cycle dies
	// at the PROVISION deadline.
	if !strings.Contains(watcherScriptWindows, "0xFF -and $h[1] -eq 0xFE") {
		t.Error("watcher script must detect a UTF-16LE BOM")
	}
	if !strings.Contains(watcherScriptWindows, "[Text.Encoding]::Unicode") {
		t.Error("watcher script must decode UTF-16LE via [Text.Encoding]::Unicode")
	}
	if !strings.Contains(watcherScriptWindows, "GetString($buf, 0, $r)") {
		t.Error("watcher script must decode the drained bytes with the detected encoding")
	}
	if !strings.Contains(watcherScriptWindows, `$avail -= ($avail % 2)`) {
		t.Error("watcher script must align a two-byte-encoding drain window to whole code units")
	}
}

// The windows debug recorder needs two different mechanisms, one per SSH
// usage shape (issue #344): Tee-Object for a one-shot SSH_ORIGINAL_COMMAND
// exec (proven to capture a piped child process's output where
// Start-Transcript does not), and an unpiped, nested Start-Transcript session
// for an interactive shell (proven to capture the prompt and typed input,
// which piping would sever from the operator's own stdin).
func TestDebugRecorderScriptWindowsShape(t *testing.T) {
	if !strings.Contains(debugRecorderScriptWindows, "if ($env:SSH_ORIGINAL_COMMAND)") {
		t.Error("windows recorder must branch on SSH_ORIGINAL_COMMAND")
	}
	if !strings.Contains(debugRecorderScriptWindows, `& $env:ComSpec /c $env:SSH_ORIGINAL_COMMAND 2>&1 | Tee-Object -FilePath $log -Append`) {
		t.Error("windows recorder's one-shot branch must pipe the child's combined output through Tee-Object")
	}
	if !strings.Contains(debugRecorderScriptWindows, `Start-Transcript -Path '`+debugSessionLogFileWindows+`' -Append`) {
		t.Error("windows recorder's interactive branch must run Start-Transcript")
	}
	// The interactive branch must NOT pipe the nested shell — piping would
	// sever the operator's stdin from it, losing the prompt and everything
	// they type.
	if strings.Contains(debugRecorderScriptWindows, `-NoExit -Command "Start-Transcript -Path '`+debugSessionLogFileWindows+`' -Append | Out-Null" |`) {
		t.Error("windows recorder's interactive branch must not pipe the nested Start-Transcript session")
	}
	if !strings.Contains(debugRecorderScriptWindows, "exit $LASTEXITCODE") {
		t.Error("windows recorder must propagate the executed branch's own exit code")
	}
}

// installDebugKeyScriptWindows must write the recorder to
// debugRecorderScriptPathWindows, wrap the authorized_keys line with a
// command= forced-command pointing at it (restrict,pty, mirroring the POSIX
// line's own options), and read back the FULL wrapped line — not just the
// bare key — to prove the wrapper itself landed.
func TestInstallDebugKeyScriptWindowsWrapsForcedCommand(t *testing.T) {
	script := fmt.Sprintf(installDebugKeyScriptWindows, "AAAA key", debugRecorderScriptWindows)
	if !strings.Contains(script, `Set-Content -Path '`+debugRecorderScriptPathWindows+`' -Value @'`) {
		t.Errorf("install script must write the recorder to %s:\n%s", debugRecorderScriptPathWindows, script)
	}
	if !strings.Contains(script, debugRecorderScriptWindows) {
		t.Errorf("install script must embed the recorder script verbatim:\n%s", script)
	}
	if !strings.Contains(script, "'@ -ErrorAction Stop") {
		t.Errorf("install script must fail loudly if writing the recorder fails, not fall through to a passing readback:\n%s", script)
	}
	wantWrapper := `command="powershell -NoProfile -File ` + debugRecorderScriptPathWindows + `",restrict,pty `
	if !strings.Contains(script, wantWrapper) {
		t.Errorf("install script missing forced-command wrapper %q:\n%s", wantWrapper, script)
	}
	// The read-back must prove the WRAPPED line landed, not just the bare key —
	// otherwise a sshd that silently ignored the command= prefix would still
	// read back "successful".
	if !strings.Contains(script, "-SimpleMatch $line") {
		t.Errorf("install script must read back the full wrapped $line, not the bare key:\n%s", script)
	}
}

// pullDebugSessionScriptWindows cannot assume UTF-8: Tee-Object/Out-File's
// default windows PowerShell 5.1 encoding is UTF-16LE with a BOM, measured
// directly off a real recorded session. It must detect the BOM via
// StreamReader's own detection (not a hand-rolled sniff) and write through
// the raw stdout stream, never the OEM-codepage [Console]::Out TextWriter.
func TestPullDebugSessionScriptWindowsDecodesBOM(t *testing.T) {
	if !strings.Contains(pullDebugSessionScriptWindows, "New-Object IO.StreamReader($fs, [Text.Encoding]::UTF8, $true)") {
		t.Error("pull script must read via StreamReader with BOM detection enabled")
	}
	// ReadWrite sharing: this read can race a still-live operator session
	// (Start-Transcript's handle still open) — a default-shared open would
	// throw on a sharing violation instead of returning what was captured
	// so far.
	if !strings.Contains(pullDebugSessionScriptWindows, `[IO.File]::Open($log, 'Open', 'Read', 'ReadWrite')`) {
		t.Error("pull script must open the log with explicit ReadWrite sharing to tolerate a still-live recorder")
	}
	if !strings.Contains(pullDebugSessionScriptWindows, "OpenStandardOutput()") {
		t.Error("pull script must write to the standard-output stream")
	}
	if strings.Contains(pullDebugSessionScriptWindows, "[Console]::Out.Write") {
		t.Error("pull script must not write through [Console]::Out (OEM codepage re-encoding)")
	}
	if !strings.Contains(pullDebugSessionScriptWindows, "if (-not (Test-Path $log)) { exit 0 }") {
		t.Error("pull script must exit cleanly when no session was ever recorded")
	}
}
