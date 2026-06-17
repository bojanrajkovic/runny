# Deploying runnyd

How runnyd is installed on a macOS host and how to migrate a host from
another runner manager.

## Why this is not just `launchctl load`

runnyd boots guests on Apple's NAT/vmnet network and reaches them over SSH on
the `192.168.64.0/24` subnet. macOS gates that behind the **Local Network**
privacy permission:

- A **system LaunchDaemon** or a **background-reparented** runnyd is *silently
  denied* vmnet access — every guest dial fails `connect: no route to host`
  while the host shell reaches the same address.
- A **foreground child of `sshd`** inherits sshd's exemption.
- A **LaunchAgent in a GUI login session** can show the one-time Local Network
  prompt; once accepted, the grant sticks (it survives binary upgrades).

So runnyd installs as a **per-user LaunchAgent**, not a LaunchDaemon. If
guests are unreachable, see "Troubleshooting: Local Network permission".

## Prerequisites

- macOS on Apple Silicon, logged into a **GUI session** for the first install
  (a desktop login, not a headless host — the TCC prompt only renders there).
- `runnyd` codesigned with the `com.apple.security.virtualization` entitlement.
  Ad-hoc signing boots VMs fine (`tools/sign/runnyd.entitlements`, ADR-0008);
  Developer ID is only needed for distribution.
- `~/.runny/config.yaml` with at least one pool, valid GitHub App credentials,
  and the runner-administration permission (`runnyd -doctor` asserts it).

The runny home is fixed at `~/.runny`, derived from the run-user's `$HOME`.
There is no `RUNNY_HOME` environment variable and no `--home` flag — the
LaunchAgent, the brew service, and the daemon all derive the home the same way.
An operator who previously set `RUNNY_HOME=/custom` must **relocate**
`config.yaml` and the file referenced by `private_key_path` into `~/.runny`
(there is no automatic migration); a stale `RUNNY_HOME` left in the environment
is ignored, and a daemon that finds no `config.yaml` under `~/.runny` fails
loudly at startup.

## Install (for testing, from this checkout)

```sh
RUNNYD=$(pwd)/bazel-bin/cmd/runnyd/runnyd_/runnyd \
  ./tools/deploy/install.sh        # writes the LaunchAgent, bootstraps it
runnyctl doctor                    # the running daemon's checks, incl. local-network
./tools/deploy/uninstall.sh        # tear down (leaves ~/.runny intact)
```

Run `install.sh` from a GUI session, not a bare SSH shell. The agent label is
`com.coderinserepeat.runnyd`; stop it with `launchctl bootout gui/$(id -u)/com.coderinserepeat.runnyd`,
never by killing the process (KeepAlive would respawn it — that is deliberate,
it is what makes the ADR-0012 wedge restart and the ADR-0014 config reload
work).

## Production install (via the tap)

Install through the Homebrew tap (the formula installs both `runnyd` and
`runnyctl`, and its `service` block is the LaunchAgent — same shape as
`tools/deploy/`):

```sh
brew install bojanrajkovic/tap/runny
# write ~/.runny/config.yaml, then from a GUI login session, WITHOUT sudo
# (sudo would install a Local-Network-denied LaunchDaemon):
brew services start runny
```

The release workflow regenerates the formula from `tools/deploy/runny.rb.tmpl`
on every release and pushes it to the tap, authenticating as the **release
bot App** (the `RELEASER_APP_ID` variable + `RELEASER_APP_PRIVATE_KEY` secret) with a
short-lived installation token scoped to `homebrew-tap`; it no-ops until those
secrets exist. That App is deliberately *not* the runtime runner-registration
App — release/CI and prod-host/runner-admin are separate blast radii.
`tools/deploy/install.sh` remains the path for running a from-checkout build.

## The Runny app and the command-line tool

The `Runny.app` bundle carries signed copies of `runnyd` and `runnyctl`, and its
Settings pane can vend the CLI: **Install command-line tool** symlinks the bundled
`runnyctl` to `/usr/local/bin/runnyctl`, so the app and the CLI it installs are
always the same build. It tries an unprivileged link first and raises a single
admin prompt only when `/usr/local/bin` needs it; it refuses to overwrite a
`brew`-managed `runnyctl` (naming the conflict) and refuses to run from a
translocated app (move Runny to your Applications folder first). The same pane
removes the link.

The app does **not** yet install or manage the daemon — `runnyd` still runs under
the Homebrew service or the manual LaunchAgent above. A bundled `runnyctl` placed
on PATH can lag a `brew`-managed `runnyd`; when it does, `runnyctl` prints a
one-line version-skew warning to stderr before its output (warn, never refuse).

## Applying config changes

runnyd reads `~/.runny/config.yaml` once, at startup. To apply an edit
gracefully:

```sh
# edit ~/.runny/config.yaml, then:
runnyctl reload -reason "why"
# or, unix muscle memory (same validated path, verdict in the daemon log):
launchctl kill SIGHUP gui/$(id -u)/com.coderinserepeat.runnyd
```

What happens (ADR-0014):

- **Validation first.** The reload re-runs every startup check against the
  new file (parse, GitHub client construction per pool, the full doctor
  suite). If the respawned daemon would refuse to start, the reload is
  refused with the failing checks and the running daemon is untouched.
- **Drain, then respawn.** On acceptance every slot drains to a stable
  idle — **running jobs finish first** — then runnyd exits and launchd
  (KeepAlive, which is load-bearing here as for the wedge restart)
  cold-starts it on the new config. `runnyctl status`/`watch` show a
  `DRAINING` banner naming the cause and the holdout slots; a multi-hour
  job legitimately holds the reload (`runnyctl recycle <slot>` is the
  explicit override — the system never kills a job on its own).
- **Operator pauses do not survive the respawn.** Pause is in-memory; the
  acceptance output lists any paused slots, and a `runnyctl pause` issued
  mid-drain warns about it.
- **Don't re-edit the file mid-drain.** If the edited file still parses,
  the respawn validates and loads it (a hash-change WARN lands in the
  daemon log); if it no longer parses, the drained daemon **holds** — it
  refuses to exit onto a file the respawn would refuse, keeps serving
  status with the hold annotation, and periodically revalidates, so fixing
  the file is sufficient.
- `runnyctl doctor` includes a `config-drift` check, so "the file differs
  from the running config" is visible before anyone wonders why behavior
  doesn't match it.

A foreground `runnyd` (no launchd) exits after the drain with
`restarting after drain: …` and stays down — the respawn is launchd's job.
Relatedly, since SIGHUP is claimed for reload, a foreground runnyd whose
terminal closes now drains gracefully instead of dying instantly;
SIGINT/SIGTERM still shut down.

The blunt fallback remains: `runnyctl pause` every slot, wait until each
sits paused in BACKOFF (`runnyctl status`), then
`brew services restart runny`. Same effect, no validation.

## Troubleshooting: Local Network permission

Symptom: every cycle dies at `AWAIT_SSH` with `connect: no route to host`,
and the daemon never provisions a VM. Confirm with `runnyctl doctor` while a
guest is booting — it must show `local-network ok`. (Use `runnyctl doctor`,
which asks the *running daemon*; `runnyd -doctor` probes from your shell,
which over SSH inherits sshd's exemption and will say `ok` even when the
daemon is denied.)

If `local-network` is not ok:

1. Open **System Settings → Privacy & Security → Local Network**. If `runnyd`
   is listed, enable it, then `brew services restart runny`.
2. If it is not listed, the daemon has never run where macOS could ask. From
   a terminal **in the GUI session** — not over SSH, not inside tmux/screen —
   run `brew services restart runny`, wait for a guest to boot, and accept
   the **"runnyd would like to find and connect to devices on your local
   network"** prompt.
3. Never start it with `sudo`: `sudo brew services` installs a LaunchDaemon,
   which macOS denies without ever prompting. `sudo brew services stop runny`,
   then start it without sudo from the GUI session.
4. To isolate the permission from other network problems: run `runnyd` in the
   foreground of an interactive SSH session. That context is exempt — if
   guests provision there but not under the LaunchAgent, the permission is
   the problem.

## Troubleshooting: SSH into a guest

Hardened guests (the default, `ssh_hardening: rotate` — see
[security.md](security.md) "Guest access") **refuse password SSH** for the
rest of the cycle: mid-cycle `ssh admin@<guest-ip>` fails with
`Permission denied` by design, and the per-cycle private key lives only in
runnyd's memory. For interactive debugging, set `ssh_hardening: off` on the
pool and apply it with **`runnyctl reload`** (see "Applying config changes" —
runnyd reads config once at startup, so a recycle alone keeps the old
setting, and a bare `brew services restart runny` kills in-flight jobs),
then SSH into a fresh guest with the pool password. Re-enable hardening and
reload again when done. (On-demand operator key injection into a live hardened guest is
tracked in [#39](https://github.com/bojanrajkovic/runny/issues/39).)

## Migrating from another runner manager

On a host already serving runners through something else:

1. Install runnyd and write `config.yaml` with the equivalent pool(s). Keep
   the old manager's launchd plist on disk for rollback.
2. `runnyd -doctor` — every check green, including `runner-perm:` and (with a
   guest up) `local-network`.
3. Stop the old manager (`launchctl bootout` its job; do not delete its plist
   yet) so the two don't contend for the macOS guest cap.
4. Start runnyd, confirm its runners show **online** in the GitHub runner list.
5. **Soak:** watch a few real jobs run end to end (pull → boot → provision →
   JIT-register → run → teardown), and `runnyctl why` any failures, with the
   old manager's plist staged for rollback.
6. Once satisfied, permanently disable the old manager's launchd job and
   archive its plist.

### Rollback

`./tools/deploy/uninstall.sh` (or `brew services stop runny`), then
re-bootstrap the old manager's plist. runnyd leaves no durable state that
interferes — `~/.runny/vms` is swept on every start, and JIT runner
registrations self-remove or are swept on the next cold start.
