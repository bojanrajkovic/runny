# internal/guest — AI Agent Notes

`Guest` (`guest.go`) adapts an `sshx.Client` to `statemachine.Guest`: what to
*do* over an already-authenticated SSH session, per guest OS. darwin/linux
are POSIX shell exec'd via `sshx.Client.Output`/`Start`; windows is a
different shape entirely — see `docs/architecture/runnyd.md`'s "Windows
guests" section for the launcher hand-off contract and why. This doc is sharp
edges only.

## Sharp edges

- **`perOS` is a loud three-way dispatch, not a convenience default.** It
  returns an error for anything other than `home.OSDarwin`/`OSLinux`/
  `OSWindows` — no silent fallback to darwin. `home.validate()` already gates
  every config-declared pool `os`, so a `perOS` error can only mean this
  package computed a bad `goos` itself; treat it as a bug to fix, not a case
  to swallow.
- **Windows commands beyond a trivial cmd one-liner MUST go through
  `encodedCommand` (`-EncodedCommand`), never inline quoting.** The guest's
  default SSH shell is cmd.exe and the scripting target is PowerShell 5.1;
  stacking ssh → cmd.exe → PowerShell quoting rules mangles anything with
  embedded quotes or newlines. `encodedCommand` UTF-16LE-encodes the script
  and base64's it — empirically the only reliable pattern. `move /Y` and the
  extract's `tar -xf` stay plain cmd one-liners (no embedded quoting to
  mangle); everything else (rotate, stop, diag pull, the watcher, key
  install) goes through it.
- **`administrators_authorized_keys`' ACL is load-bearing, not optional.**
  Windows OpenSSH silently ignores that file for an Administrators-group
  account unless its ACL is stripped to `SYSTEM:F` + `BUILTIN\Administrators:F`
  (`icacls ... /inheritance:r /grant ...`) right after writing it. Skipping
  the `icacls` call doesn't error anywhere — the key install "succeeds" and
  the key silently never authenticates. Both `rotateScriptWindowsTemplate`
  and `installDebugKeyScriptWindows` carry it; don't drop it from either.
- **`PasswordAuthentication no` is PREPENDED to `sshd_config`, never
  appended.** sshd_config is first-match-wins, and the stock Windows
  `sshd_config` ends with a `Match Group administrators` block; appending
  after it lands inside that block (or loses to whatever precedes it)
  instead of applying globally. `rotateScriptWindowsTemplate` builds the new
  content as `directive + existing`, not `existing + directive` — keep it
  that way.
- **The sshd service restart runs DETACHED, never inline.** Windows sshd
  connections are children of the service process (unlike Linux's
  per-connection socket activation or systemd reload) — restarting it from
  inside the session issuing the restart kills that very session before it
  can report success. `rotateScriptWindowsTemplate`'s last line spawns a
  separate `Start-Process` that sleeps 1s then calls `Restart-Service sshd`,
  and the script's own exit lands before the restart does. The reconnect
  that follows relies on `sshx.WaitFor`'s own retry loop to ride out the gap
  where the old sshd may still be answering.
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
- **Windows debug sessions are not recorded (v1).** `InstallAuthorizedKey`'s
  windows branch installs the key with the ACL fix but no `command=`
  transcription wrapper — the POSIX recorder (`debugRecorderDarwin`/`Linux`)
  is a `script(1)` wrapper, and Windows has no `script(1)`. It logs loudly at
  install time rather than silently handing out an unrecorded session;
  `PullDebugSession` returns `nil, nil` for a windows guest without dialing.
