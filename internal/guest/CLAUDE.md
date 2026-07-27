# internal/guest — AI Agent Notes

`Guest` (`guest.go`) adapts an `sshx.Client` to `statemachine.Guest`: what to
*do* over an already-authenticated SSH session, per guest OS. darwin/linux
are POSIX shell exec'd via `sshx.Client.Output`/`Start`; windows is a
different shape entirely — see `docs/architecture/runnyd.md`'s "Windows
guests" section for the launcher hand-off contract and why. This doc is sharp
edges only.

## Sharp edges

- **The package splits by guest-OS dialect: `guest.go` (shared machinery +
  the dispatcher methods), `guest_posix.go` (darwin/linux scripts and
  helpers), `winguest.go` (the windows dialect).** `winguest.go` is
  deliberately not `guest_windows.go` — Go's implicit `_windows` GOOS file
  suffix would constrain it to windows-only hosts, and this code must build
  and test on every host (the FSM tests exercise the windows dialect on
  macOS/Linux CI). Same rule for `winguest_test.go` vs `guest_test.go`. A
  declaration belongs in the dialect file it's used only by; anything a
  dispatcher method needs from both dialects stays in `guest.go`.
- **`perOS` picks only between the two POSIX scripts (darwin/linux), and
  errors on anything else — windows included.** Every windows call site
  (`StartRunner`, `Rotate`, `PushRunnerTarball`, `StopRunner`, `PullDiag`,
  `InstallAuthorizedKey`, `PullDebugSession`) branches to its own
  windows-specific path on `goos == home.OSWindows` *before* any `perOS`
  call, so `perOS` never needs a third value. `home.validate()` already gates
  every config-declared pool `os`, so a `perOS` error can only mean this
  package computed a bad `goos` itself; treat it as a bug to fix, not a case
  to swallow — including by reflexively adding a windows branch back into
  `perOS` instead of dispatching earlier.
- **Every windows guest *command* goes through `encodedCommand`
  (`-EncodedCommand`); every windows *file transfer* goes through scp-sink
  framing (`scpSinkCommand`/`scpSource`). Nothing else.** Commands: the
  guest's default SSH shell is cmd.exe and the scripting target is
  PowerShell 5.1; stacking ssh → cmd.exe → PowerShell quoting rules mangles
  anything with embedded quotes or newlines, and `encodedCommand`
  (UTF-16LE + base64) is empirically the only reliable pattern. It also
  prepends `$ProgressPreference='SilentlyContinue'` — PS 5.1's console-less
  host serializes progress records (e.g. module autoload's "Preparing
  modules for first use") as `#< CLIXML` blobs on stderr, which broke
  host-key parsing on the real image. Transfers: never stream bytes into a
  PowerShell stdin read (`[Console]::OpenStandardInput()`); the PS host
  interposes on redirected stdin and, hardware-proven, either wedges the
  session until the state deadline or gets the connection killed. `scp -t`
  against the guest's native scp.exe is the byte channel — a blind
  single-file stream whose exit code is the failure signal.
- **`administrators_authorized_keys`' ACL is load-bearing, not optional, and
  has exactly one home: `psAppendAuthorizedKeyLine`.** Windows OpenSSH
  silently ignores that file for an Administrators-group account unless its
  ACL is stripped to `SYSTEM:F` + `BUILTIN\Administrators:F`
  (`icacls ... /inheritance:r /grant ...`) right after writing it — and
  icacls itself can fail, so the fragment also checks `$LASTEXITCODE` and
  exits loud (2) rather than trusting `| Out-Null`'d success. Both
  `rotateScriptWindowsTemplate` (the cycle key) and
  `installDebugKeyScriptWindows` (the operator's debug key) embed this same
  fragment; don't fork a second copy of the ACL logic into either.
- **`PasswordAuthentication no` is PREPENDED to `sshd_config`, never
  appended.** sshd_config is first-match-wins, and the stock Windows
  `sshd_config` ends with a `Match Group administrators` block; appending
  after it lands inside that block (or loses to whatever precedes it)
  instead of applying globally. `rotateScriptWindowsTemplate` builds the new
  content as `directive + existing`, not `existing + directive` — keep it
  that way.
- **The sshd service restart runs INLINE, not detached.** A detached
  `Start-Process` (sleep, then `Restart-Service sshd`) was tried first, on
  the theory that an inline restart would kill the session issuing it —
  hardware-proven wrong against the real image: the detached child never
  actually reached `Restart-Service` (the service's PID never changed
  across two separate attempts), while a direct inline `Restart-Service`
  produced a new PID immediately and left the issuing session alive to
  report its own exit status. It wouldn't matter even if some other image's
  sshd DOES drop the session here — `rotateWindows` discards this connection
  either way and reconnects fresh with the new key (`sshx.WaitFor`), so
  there is nothing about this session's survival worth protecting. The
  restart carries `-ErrorAction Stop` so a failure aborts the script loudly
  instead of leaving the old password/key state live while rotation reports
  success.
- **Windows guests only ever push, never mount.** `hcsMachine.NeedsRunnerPush()`
  is unconditionally true for a windows guest (it only ever boots on the HCS
  host — see `internal/vm/CLAUDE.md`), so there is no live-share variant of
  the windows provision path to keep in sync with the pushed one, unlike
  linux's `provisionScriptLinux`/`provisionScriptLinuxPushed` split.
- **`provisionScript` refuses `goos == "windows"` outright.** A windows guest
  never takes the POSIX exec-one-script path — `StartRunner` dispatches it to
  `startRunnerWindows` before `provisionScript` is ever called. Don't "fix"
  `provisionScript` to grow a windows branch; the windows launch is a
  multi-step hand-off (extract, JIT delivery, watcher start), not a
  string-template swap.
- **The watcher `Proc` is the runner, as far as the FSM is concerned.**
  `startRunnerWindows`'s returned `Proc` isn't the runner process itself (the
  image's launcher owns that, invisibly) — it's a PowerShell session that
  tails `C:\runny\runner.log` and exits with the runner's exit code once
  `C:\runny\runner-exit.txt` appears. It tolerates the log not existing yet
  (the launcher may not have picked up `.jitconfig` at the moment the watcher
  starts); PROVISION's own deadline is what bounds a launcher that never
  starts, not the watcher itself.
- **The watcher decodes `runner.log` by its BOM and re-emits UTF-8; it does
  NOT forward raw bytes.** The launcher writes the log via PowerShell 5.1's
  `*>` redirect, which encodes as UTF-16LE (Windows' native "Unicode") — so
  the on-disk bytes are `L\0i\0s\0t...`, and the FSM's UTF-8
  `strings.Contains(line, "Listening for Jobs")` match can never fire against
  them. This was the bug that made every windows cycle die at the PROVISION
  deadline with a healthy, listening runner. The watcher detects the BOM
  (UTF-16LE/BE, UTF-8, or none) once, decodes each drained chunk with that
  encoding, and writes UTF-8 to `[Console]::OpenStandardOutput()` (still the
  raw stream, never the OEM-codepage `[Console]::Out` TextWriter). Keyed off
  the BOM rather than assuming one encoding on purpose: UTF-16LE is the
  platform default any Windows tool may reach for, and the daemon is the one
  consumer that must be right regardless of which wrote the file. Two-byte
  encodings align the drain window to whole code units (`$avail - $avail % 2`)
  so a poll can't split a UTF-16 unit; a lone surrogate at a boundary (astral
  chars, absent from runner logs) degrades to U+FFFD. The exit-code read is
  guarded by an
  integer regex (`$code -match '^-?\d+$'`) before the `[int]` cast — the
  sign matters (a crashed process reports a negative NTSTATUS exit code;
  digits-only would loop forever on exactly the exits that most need
  reporting), and the guard is defense in depth, since the 250ms
  settle-then-redrain before reading already makes an empty/partial read
  practically unreachable.
- **Every windows read of a file the guest is still writing opens with
  explicit `ReadWrite` sharing, and emits through
  `[Console]::OpenStandardOutput()` rather than `[Console]::Out`.** All three
  readers do both: `watcherScriptWindows`'s `Detect`/`Drain`,
  `pullDebugSessionScriptWindows`, and `pullDiagScriptWindows`. Windows file
  sharing is mutual, which is the counter-intuitive half — a reader declaring
  `FileShare.Read`, which is what `[IO.File]::ReadAllBytes` and `Get-Content`
  open with, refuses to coexist with an existing handle's `Write` access, so
  it throws against a live writer *even when that writer declared
  `FileShare.ReadWrite`* (measured on a windows host across all three writer
  share modes; only a `FileShare.None` writer defeats
  `[IO.File]::Open($p,'Open','Read','ReadWrite')`). Every one of these reads
  races a live writer by design, not by accident: teardown pulls diag
  *before* `StopRunner`, so the runner still holds `_diag\Runner_*.log`, and
  a debug session is pulled while the operator may still be connected.
  `[Console]::Out` is a TextWriter that re-encodes to the console's OEM
  codepage and turns any non-ASCII into `?`. A failure here is quiet —
  PowerShell reports the sharing violation as non-terminating, so the loop
  continues and the script still exits 0; `Output` folds stderr into the same
  buffer, so the exception text lands in the artifact where the file's
  contents should have been.
- **Windows debug session recording needs two mechanisms, not one, because
  no single windows mechanism covers both SSH usage shapes.**
  `debugRecorderScriptWindows`'s doc comment has the full rationale; the
  short version: `Tee-Object` piping a `cmd.exe` child captures a one-shot
  `SSH_ORIGINAL_COMMAND` exec's output (proven where `Start-Transcript`
  silently drops it), but piping would sever an interactive session's stdin,
  so the interactive branch instead spawns an unpiped nested
  `powershell -NoExit` with `Start-Transcript` already running inside it.
  `Start-Transcript` itself doesn't capture a native program's console
  output (`WriteConsole` bypasses the `.NET Console.Out` hook it relies on)
  — proven on a real Windows OpenSSH host, not assumed — so an interactive
  session's build-tool output isn't guaranteed to land; don't "fix" this by
  merging the two branches into one mechanism, there isn't one that does
  both. `PullDebugSession`'s windows read decodes the log via
  `StreamReader`'s own BOM detection (`Tee-Object`/`Out-File` default to
  UTF-16LE on PS 5.1), never assumes UTF-8.
