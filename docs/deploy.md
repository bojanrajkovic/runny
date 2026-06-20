# Deploying runnyd

How runnyd is installed on a macOS host and how to migrate a host from
another runner manager.

## Why this is not just `launchctl load`

runnyd boots guests on Apple's NAT/vmnet network and reaches them over SSH on
the `192.168.64.0/24` subnet. macOS gates that behind the **Local Network**
privacy permission, and how a process is launched decides how that gate
behaves:

- macOS keeps **separate** local-network privacy state per user account. A
  per-user **LaunchAgent** is therefore subject to a one-time prompt — shown
  only in a GUI login session — after which the grant sticks (it survives
  binary upgrades). This is the path runnyd installs today.
- A **foreground child of `sshd`** inherits sshd's exemption and reaches guests
  with no prompt — handy for interactive debugging.
- A runnyd that **self-daemonizes or reparents** away from launchd is neither a
  launchd job nor an sshd child, and is *silently denied* — every guest dial
  fails `connect: no route to host` while the host shell reaches the same
  address. runnyd never backgrounds itself (crash-only KeepAlive keeps it a
  launchd child); don't wrap it in something that does.

So runnyd installs **started by launchd**, in one of two shapes: a **non-root
system LaunchDaemon** for headless fleets (no GUI session and no prompt — a
launchd-started daemon of any uid is auto-allowed; see "Headless system daemon")
or a **per-user LaunchAgent** for a desktop, started in your GUI session so the
one-time per-account Local Network prompt can appear. The one shape that does
not work is a runnyd that backgrounds itself off launchd. If guests are
unreachable, see "Troubleshooting: Local Network permission".

## Prerequisites

- macOS on Apple Silicon, logged into a **GUI session** for the first install
  (a desktop login, not a headless host — the TCC prompt only renders there).
- `runnyd` codesigned with the `com.apple.security.virtualization` entitlement.
  Ad-hoc signing boots VMs fine (`tools/sign/runnyd.entitlements`, ADR-0008);
  Developer ID is only needed for distribution.
- `~/.runny/config.yaml` with at least one pool, valid GitHub App credentials,
  and the runner-administration permission (`runnyd -doctor` asserts it).

The runny home is **deployment-resolved, not configurable**: a non-root system
daemon uses `/Library/Application Support/runny` (see "Headless system daemon"),
and a per-user agent uses `~/.runny` derived from the run-user's `$HOME`. There
is no `RUNNY_HOME` environment variable and no override — the daemon and its
clients (`runnyctl`, the app) resolve the same home, so they can never disagree
about where the socket and credentials live. A daemon that finds no
`config.yaml` in its home fails loudly at startup. (This guide's per-user
sections write `~/.runny`; for a system daemon read that as
`/Library/Application Support/runny`.)

## Authoring `config.yaml`

A JSON Schema for the config file ships at
[`tools/configschema/config.schema.json`](../tools/configschema/config.schema.json),
generated from the `home.Config` struct (a golden test keeps the two from
drifting). Point your editor at it for autocomplete, key-typo detection, and
inline validation of the enums and required keys.

Add a modeline to the top of `~/.runny/config.yaml` — the
[YAML Language Server](https://github.com/redhat-developer/yaml-language-server)
(bundled with the VS Code *YAML* extension and most LSP setups) reads it:

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/bojanrajkovic/runny/main/tools/configschema/config.schema.json
```

Or, without touching the file, map it in VS Code `settings.json`:

```json
"yaml.schemas": {
  "https://raw.githubusercontent.com/bojanrajkovic/runny/main/tools/configschema/config.schema.json": "**/.runny/config.yaml"
}
```

A minimal config (one org-scoped macOS pool), modeline included:

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/bojanrajkovic/runny/main/tools/configschema/config.schema.json
pools:
  - name: mac
    os: darwin
    image: ghcr.io/cirruslabs/macos-sequoia-xcode:latest
    count: 2
    target:
      org: my-org
    github:
      app_id: 123456
      private_key_path: ~/.runny/runner-app.pem
```

The schema describes the file's **shape** — keys, types, enums, the
org-or-owner/repo target. The **semantic** rules it can't cleanly express
(duration positivity, runner-name length, the macOS guest cap) stay with the
daemon: `runnyd -doctor` and the load-time validation remain authoritative, and
a config that passes the schema can still be refused there with a precise error.

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

Install through the Homebrew tap. The formula is **delivery-only** — it installs
the `runnyd` and `runnyctl` binaries but registers no service (Homebrew's
`service` DSL can only make a per-user LaunchAgent, while the headless install is
a non-root system LaunchDaemon — see "Headless system daemon"):

```sh
brew install bojanrajkovic/tap/runny
sudo runnyctl install-daemon   # register the non-root system LaunchDaemon
```

The release workflow regenerates the formula from `tools/deploy/runny.rb.tmpl`
on every release and pushes it to the tap, authenticating as the **release
bot App** (the `RELEASER_APP_ID` variable + `RELEASER_APP_PRIVATE_KEY` secret) with a
short-lived installation token scoped to `homebrew-tap`; it no-ops until those
secrets exist. That App is deliberately *not* the runtime runner-registration
App — release/CI and prod-host/runner-admin are separate blast radii.
`tools/deploy/install.sh` remains the path for running a from-checkout per-user
agent.

## Headless system daemon

For a headless fleet host — no desktop login — runnyd runs as a **non-root
system LaunchDaemon** under a dedicated service account. One privileged step
installs it; the daemon then runs unprivileged.

```sh
brew install bojanrajkovic/tap/runny
sudo runnyctl install-daemon
# then land config + the App key in the system home — your account has write
# access via an inheriting ACL, so no sudo is needed for edits:
$EDITOR "/Library/Application Support/runny/config.yaml"
cp runner-app.pem "/Library/Application Support/runny/"
runnyctl doctor
```

`runnyctl install-daemon` (one `sudo`):

- Creates a hidden, home-less service account (`_runny`): no login, no shell, no
  home directory — its entire state lives in the system home.
- Creates `/Library/Application Support/runny` owned by `_runny`, mode `0700`,
  with a **dual inheriting ACL**: your operator account gets directory write
  (edit config, land the key, read artifacts) and `_runny` gets read (so the
  daemon can read your operator-owned `0600` config and key). The home holds
  everything — config, the App key, logs, images, VM clones, cycle artifacts,
  and the control socket.
- Writes `/Library/LaunchDaemons/com.coderinserepeat.runnyd.plist`
  (`UserName=_runny`, `KeepAlive`) and `launchctl bootstrap system`.

The daemon starts immediately and **crash-loops loudly until a valid
`config.yaml` is present** (visible in `logs/launchd.err.log` under the home, or
via `runnyctl doctor`); it comes up on the next restart once the config lands.
There is no GUI prompt — a launchd-started daemon of any uid is auto-allowed
Local Network access.

The plist points at the `runnyd` beside `runnyctl` (the brew opt symlink, kept
current by `brew upgrade`). From a checkout, where the built binaries are not
co-located, use the script — it stages them and delegates to `install-daemon`:

```sh
sudo RUNNYD=$(pwd)/bazel-bin/cmd/runnyd/runnyd_/runnyd \
     RUNNYCTL=$(pwd)/bazel-bin/cmd/runnyctl/runnyctl_/runnyctl \
     ./tools/deploy/install-system.sh
```

Remove it with `sudo runnyctl uninstall-daemon` (or
`sudo ./tools/deploy/uninstall-system.sh`): it verifies the job is actually
unloaded (refusing to proceed over a still-running daemon), then removes the
plist **and the home**. The home is purged on purpose — a left-behind home would
keep winning client resolution (so a later per-user agent would be unreachable)
and would leave the App key at rest after you believe runny is gone. The `_runny`
account is kept, so a reinstall reuses its uid and the recreated home's ownership
stays valid. **Back up `config.yaml` first if you want to keep it.**

Stop the daemon with `sudo launchctl bootout system/com.coderinserepeat.runnyd`,
never by killing it: KeepAlive respawns it, which is what the ADR-0012 wedge
restart and the ADR-0014 reload depend on.

## The Runny app and the command-line tool

The `Runny.app` bundle carries signed copies of `runnyd` and `runnyctl`, and its
Settings pane can vend the CLI: **Install command-line tool** symlinks the bundled
`runnyctl` to `/usr/local/bin/runnyctl`, so the app and the CLI it installs are
always the same build. It tries an unprivileged link first and raises a single
admin prompt only when `/usr/local/bin` needs it; it refuses to overwrite a
`brew`-managed `runnyctl` (naming the conflict) and refuses to run from a
translocated app (move Runny to your Applications folder first). The same pane
removes the link.

The app can also **install and manage the daemon** as a per-user LaunchAgent —
the desktop-GUI install channel, beside the Homebrew service for headless fleets.
From a copy of Runny in `/Applications`:

- **Settings → Daemon → "Start runnyd at login"** registers the bundled `runnyd`
  via `SMAppService` (one confirmation names the launchd label). The first guest
  boot raises the **Local Network** prompt; the app surfaces a grant card
  proactively if the grant is missing or pending, *before* a guest dial fails (the
  per-user agent is subject to that one-time prompt — see "Why this is not just
  `launchctl load`" above).
- A **Start** affordance appears in the menu bar and main window when the agent is
  installed but the daemon isn't running.
- After upgrading the app to a newer build, **"Update Daemon"** drains running
  jobs, then restarts onto the freshly-bundled binary (a `runnyctl reload -wait`
  by another name).
- Toggling it off uninstalls the agent; mid-job it warns that the running job is
  abandoned.

Install requires Runny in `/Applications` (a translocated or `~/Downloads` launch
is refused, recoverably). **The two channels split by audience:** the app is the
**desktop-GUI** install path (a per-user agent in your login session); the
Homebrew service (or the manual LaunchAgent) is the **headless-fleet** path. The
app installs its agent **only when no other manager owns the daemon**: it probes
the launchd domain, and on detecting a Homebrew-managed (`homebrew.mxcl.runny`) or
manually-installed (`com.coderinserepeat.runnyd`) daemon it does **not** install —
it drops to an observer (status streams normally as a sibling client over the same
socket), replaces the install toggle with a banner naming the managing channel
("Managed by Homebrew — `brew services restart runny`"), and never displaces the
other manager. To switch a host from brew/manual to the app, remove the foreign
agent first — `brew services stop runny`, or for a manual install
`launchctl bootout gui/$(id -u)/com.coderinserepeat.runnyd 2>/dev/null; rm -f ~/Library/LaunchAgents/com.coderinserepeat.runnyd.plist`
(the `rm` is load-bearing, and separated by `;` not `&&`: `bootout` only unloads a
running job and exits nonzero when the job is already gone, but launchd reloads a
leftover plist at next login, and the app treats a persisted plist as a dormant
owner it will not install over) — then reopen Runny. A
bundled `runnyctl` on PATH can lag a `brew`-managed `runnyd`; when it does,
`runnyctl` prints a one-line version-skew warning to stderr before its output
(warn, never refuse).

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
3. Don't start it with `sudo`. `sudo brew services` runs runnyd as a **root
   LaunchDaemon** — unnecessary privilege, and a different install than the
   per-user agent the rest of this guide assumes. If you ran it that way,
   `sudo brew services stop runny`, then start it without sudo from the GUI
   session.
4. To isolate the permission from other network problems: run `runnyd` in the
   foreground of an interactive SSH session (it prints its log to the terminal
   there, in addition to the log file). That context is exempt — if guests
   provision there but not under the LaunchAgent, the permission is the problem.

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
