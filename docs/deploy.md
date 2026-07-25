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
  address. runnyd never backgrounds itself (KeepAlive restarts keep it a
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
      private_key_path: /Users/you/.runny/runner-app.pem
```

`private_key_path` is read **verbatim** — no `~` or `$HOME` expansion — so it
must be an absolute path (`internal/github/github.go`'s `os.ReadFile` gets
whatever string is in the file). `runnyctl install-daemon --config` (below)
and `runnyctl edit-config` both rewrite an authored path into the resolved
home for you; hand-authoring a config yourself still needs the literal
absolute path.

The schema describes the file's **shape** — keys, types, enums, the
org-or-owner/repo target. The **semantic** rules it can't cleanly express
(duration positivity, runner-name length, the macOS guest cap) stay with the
daemon: `runnyd -doctor` and the load-time validation remain authoritative, and
a config that passes the schema can still be refused there with a precise error.

### Pulling from a private registry

Pulls authenticate from the standard docker/oras/skopeo config file
(`$DOCKER_CONFIG/config.json`), so a public image needs nothing here. runny
only ever **reads**, and only reads a **static `auths` entry**: there is no
`runnyctl login`, and runny never writes or copies a credential.

**Credential helpers are not supported** — no `credsStore`, no `credHelpers`.
On macOS `docker login` defaults to `credsStore: "osxkeychain"`, which puts
the secret in your *login keychain* and leaves the `auths` entry an empty
stub. The system daemon runs as a service account that cannot open your
keychain at all, so a helper could never produce a credential for it no matter
where the config file lives. Rather than half-support a path that cannot work
in the deployment that needs it, runny reads the static entry only — and says
so in the log when it finds a config that delegates to a helper, instead of
pulling anonymously in silence.

Both registry challenge forms are handled: the Bearer token exchange most
hosted registries use, and plain `Basic` (a bare Distribution behind htpasswd),
where the credentials go straight back to the registry with no token endpoint
involved. Credentials only ever travel over HTTPS — the sole exception is a
loopback registry, matching the convention container tooling already uses.

**Where to put it.** A per-user agent uses the standard
`~/.docker/config.json`. The system daemon's own home is `/var/empty`, so it
defaults `DOCKER_CONFIG` to `<home>/docker`, which you can write without
`sudo` via the home's inheriting ACL.

`auth` is base64 of `username:password`:

```json
{
  "auths": {
    "registry.example.com": {
      "auth": "cm9ib3QkcnVubnk6VE9LRU4="
    }
  }
}
```

The host key must match the image ref's registry host exactly — the
`registry.example.com` in `image: registry.example.com/org/image:tag`. Port
included if the ref has one; no scheme, no trailing slash.

```console
$ D="/Library/Application Support/runny/docker"
$ mkdir -p "$D"
$ printf '{"auths":{"registry.example.com":{"auth":"%s"}}}\n' \
    "$(printf 'runny-puller:TOKEN' | base64)" > "$D/config.json"
$ chmod 600 "$D/config.json"
```

**Rotating it is editing that file** — the credential is read fresh on every
pull attempt, so a new token takes effect on the next pull with no restart and
no `runnyctl reload` (a pull already in flight finishes on the credential it
started with). Keep it a credential issued *to the daemon* (a pull-scoped
robot account), not a copy of your own: a copy is a second place to go stale,
and it grants the daemon everything your account can do.

Failures that are *misconfigurations* are logged rather than swallowed — a
`config.json` that is unreadable (a wrong ACL on the file is the usual cause)
or unparseable, and a config that holds this host's credential in a helper
rather than a static entry. Each falls back to an anonymous pull, so check the
daemon log when a private pull 401s unexpectedly. A genuinely absent config or
an unmatched host stays quiet — those are the normal public-image cases.

### Enabling OTLP telemetry

`observability` is opt-in and absent by default: with no block, runnyd emits
no telemetry and opens no OTEL SDK at all. Adding it turns on both traces and
metrics export to one collector endpoint:

```yaml
observability:
  otlp:
    endpoint: https://collector.example:4317   # https = TLS, http = insecure
    metrics_interval: 60s                        # optional; default 60s
    headers:                                     # optional; sent on every export
      x-honeycomb-team: ${env:HONEYCOMB_API_KEY}
```

Enabling it adds **outbound OTLP egress only** — the daemon still opens no
listening TCP socket; it dials out to `endpoint` the same way it dials GitHub.
`https` selects TLS, `http` is for a local/insecure collector. `endpoint` is
the single switch: a non-empty value turns on export for both signals, there
are no per-signal endpoints.

`headers` carries backend auth — every OTLP backend authenticates via a
request header (`x-honeycomb-team`, `api-key`, `authorization: Bearer …`), so
a plain map covers them all. Values may reference environment variables with
the Collector's `${env:VAR}` syntax, resolved once at config load; an unset
variable refuses the config with a precise error rather than exporting with a
silently empty credential. (Under launchd the daemon's environment is
whatever the plist's `EnvironmentVariables` provides — the expansion is most
useful for foreground `runnyd` runs and dev shells.) Only that exact placeholder shape expands — every
other byte, including bare `$`, passes through untouched. A literal key in
the file works too: the config is already an operator-owned `0600` file, the
same posture that protects the `github.private_key_path` target. See
[ADR-0024](architecture-decisions/0024-observability-event-seam.md) for the
design this enables.

Traces arrive as one span tree per runner cycle (root `runny.cycle`, a child
per FSM step, a grandchild per sub-step action); metrics as `runny.*`
instruments — cycle/job counters, duration histograms, and per-slot state
gauges. The architecture is described in
[docs/architecture/observability.md](architecture/observability.md); the
instruments themselves are documented where they're created, in
`internal/telemetry/metrics.go`.

**Series identity is resource + labels.** Metric labels carry only the pool,
the slot, and closed vocabularies; *which daemon* emitted a series rides the
OTLP resource attributes (`service.instance.id`, `host.name`). A collector
pipeline that drops resource attributes on the way to a Prometheus-family
backend will silently merge series from different hosts — preserve
`service.instance.id` (resource-to-label promotion, or a `target_info`
join) when more than one runnyd reports to the same backend.

**Histograms are exponential (native), not fixed-bucket.** Every `runny.*`
histogram (the durations and the pull-bytes one) exports as an OTLP
exponential histogram, so
a Prometheus-family backend ingests them as native histograms — there are
no `_bucket`/`le` series. The backend must have native-histogram ingestion
enabled (e.g. Prometheus `--enable-feature=native-histograms`), and
`histogram_quantile` takes the bare series, as below.

#### Querying the metrics

Recipes for the questions the instruments were designed to answer, in
PromQL (after the usual OTLP→Prometheus name mapping, `runny.cycle.count` →
`runny_cycle_count_total` and so on):

```promql
# Fleet by state — "5 in JOB": the state gauge is a 0/1 matrix per
# (pool, slot, state), so counting is a sum, not a separate metric.
sum by (pool, state) (runny_slot_state)

# Utilization: fraction of a pool's slots running a job.
sum by (pool) (runny_slot_state{state="JOB"})
  / count by (pool) (count by (pool, slot) (runny_slot_state))

# Job throughput and p95 duration (native-histogram form: no le/_bucket).
sum by (pool) (rate(runny_job_count_total[15m]))
histogram_quantile(0.95, sum by (pool) (rate(runny_job_duration_seconds[1h])))

# Cycle failure ratio. `shutdown` and `recycle` are benign truncations;
# `failure` and `wedge` read as health signals — with one caveat: a debug
# hold that expires also ends its cycle as `failure` by design, so heavy
# runnyctl debug use shows up here even though backoff treats it as benign.
sum by (pool) (rate(runny_cycle_count_total{ending=~"failure|wedge"}[1h]))
  / sum by (pool) (rate(runny_cycle_count_total[1h]))

# Real failure durations, excluding benign truncations — the duration
# histogram carries `ending` for exactly this filter.
histogram_quantile(0.95, sum by (pool) (
  rate(runny_cycle_duration_seconds{result="failure",ending!~"shutdown|recycle"}[6h])))

# Time in current state — alert when a slot sits anywhere too long
# (a stuck slot is exactly the silent failure runny exists to kill).
time() - runny_slot_state_entered_time_seconds

# Wedged slots (parked until daemon restart) and operator pauses.
runny_slot_wedged == 1
runny_slot_paused == 1

# Disk headroom on the runny home filesystem, as ENSURE_IMAGE sees it.
# If the home filesystem becomes unreadable this series goes absent (the
# daemon logs the statfs failure); alert on absent() as well as low values.
runny_home_disk_free_bytes

# Image pull health: failed pulls over the window, and p95 pull time.
# These record once per underlying pull (concurrent slots share one), so
# they count pulls, not waiting slots; duration is the pull's whole
# lifetime, including disk holds and re-attempts. A cycle's own time at
# the mercy of a pull is its wait-for-pull action — on its trace, and in
# runny_action_duration_seconds{action="wait-for-pull"}, which counts
# per waiting cycle and so double-counts against these per-pull series.
histogram_count(sum(increase(runny_image_pull_duration_seconds{outcome="error"}[6h])))
histogram_quantile(0.95, sum(rate(runny_image_pull_duration_seconds[6h])))

# Effective pull bandwidth: bytes moved over time spent, across pulls.
histogram_sum(sum(rate(runny_image_pull_bytes[6h])))
  / histogram_sum(sum(rate(runny_image_pull_duration_seconds[6h])))

# Runner-tarball downloads — cache hits record nothing, so a sustained
# rate here means runner versions are churning or the cache keeps
# getting lost.
histogram_count(sum(rate(runny_runner_tarball_download_duration_seconds[6h])))
```

Where a step or action is slow, switch to traces: the cycle's span tree
carries per-step and per-action timing with the exact error text.

## Production install (via the tap)

Install through the Homebrew tap. The formula is **delivery-only** — it installs
the `runnyd` and `runnyctl` binaries but registers no service (Homebrew's
`service` DSL can only make a per-user LaunchAgent, while the headless install is
a non-root system LaunchDaemon — see "Headless system daemon"):

```sh
brew install bojanrajkovic/tap/runny
sudo runnyctl install-daemon   # register the non-root system LaunchDaemon
```

For a **desktop, per-user** install instead, use the Homebrew cask or drag
`Runny.app` from the `.dmg` on the [Releases](https://github.com/bojanrajkovic/runny/releases) page:

```sh
brew install --cask bojanrajkovic/tap/runny-app
```

Then use **Settings → Daemon → "Start runnyd at login"** — see "The Runny app and
the command-line tool" below. The cask and formula are mutually exclusive; if
`brew install --cask runny-app` fails with a `runnyctl` symlink collision (the
formula is present), run `brew uninstall bojanrajkovic/tap/runny` first. These are
the two supported shapes, split by audience: the system LaunchDaemon
(`sudo runnyctl install-daemon`) is the headless-fleet path; the app is the
desktop path.

**Update channels:** the Homebrew cask auto-updates with `brew upgrade --cask
runny-app`. The direct-`.dmg` install has no self-updater; instead, the app
surfaces a dismissible **"Runny vX.Y.Z available"** banner in the popover when a
newer release is detected — linking to the GitHub releases page. The banner
checks on launch and every 24 hours (toggleable in **Settings → Updates**) and
can be triggered manually via **Runny menu → Check for Updates…**.

The release workflow regenerates the formula from `tools/deploy/runny.rb.tmpl`
on every release and pushes it to the tap, authenticating as the **release
bot App** (the `RELEASER_APP_ID` variable + `RELEASER_APP_PRIVATE_KEY` secret) with a
short-lived installation token scoped to `homebrew-tap`; it no-ops until those
secrets exist. That App is deliberately *not* the runtime runner-registration
App — release/CI and prod-host/runner-admin are separate blast radii.

**Beta channel:** a manually-tagged pre-release (ADR-0010) publishes a
separate `runny-beta` formula and `runny-app-beta` cask instead of
overwriting the stable pair:

```sh
brew install bojanrajkovic/tap/runny-beta                # CLI, beta channel
brew install --cask bojanrajkovic/tap/runny-app-beta      # app, beta channel
```

All four packages (`runny`, `runny-beta`, `runny-app`, `runny-app-beta`) are
mutually exclusive with each other — same binaries, and for the casks the
same `Runny.app` bundle identity — so switching channels means
uninstalling the old one first (e.g. `brew uninstall runny-beta && brew
install runny`).

## Headless system daemon

For a headless fleet host — no desktop login — runnyd runs as a **non-root
system LaunchDaemon** under a dedicated service account. One privileged step
installs it; the daemon then runs unprivileged.

Write a config first (anywhere — it's a one-shot seed, not where it ends up),
naming your App key's path as it sits on disk right now, then stage it with
`--config`:

```sh
brew install bojanrajkovic/tap/runny
sudo runnyctl install-daemon --config ./config.yaml
runnyctl doctor
```

`--config PATH` reads each pool's `github.private_key_path` as a **source** —
copies the key(s) it names into the system home, rewrites the config's copy of
each path to point at the in-home copy (a literal byte substitution, so your
schema modeline and comments survive), then runs `runnyd -test-config` against
the staged result. The daemon is bootstrapped **only on a green verdict** — a
bad config fails synchronously, at the prompt, instead of crash-looping. A
failed `-test-config` leaves the home scaffolded but not started; fix the
authored file and rerun the same command (idempotent — re-staging overwrites).
Pools sharing one App key stage it once; distinct keys that happen to share a
filename get a disambiguating suffix.

`runnyctl install-daemon` (one `sudo`, with or without `--config`):

- Creates a hidden, home-less service account (`_runny`): no login, no shell, no
  home directory — its entire state lives in the system home.
- Creates `/Library/Application Support/runny` owned by `_runny`, mode `0700`,
  with a **dual inheriting ACL**: your operator account gets directory write
  (edit config, land the key, read artifacts) and `_runny` gets read (so the
  daemon can read your operator-owned `0600` config and key). The home holds
  everything — config, the App key, logs, images, VM clones, cycle artifacts,
  and the control socket.
- Writes `/Library/LaunchDaemons/com.coderinserepeat.runnyd.plist`
  (`UserName=_runny`, `KeepAlive`) and, once validated, `launchctl bootstrap system`.

**Without `--config`**, it registers the daemon before any config exists, and
the daemon **crash-loops loudly** until you land one by hand:

```sh
sudo runnyctl install-daemon
# your account has write access via the inheriting ACL, so no sudo for edits:
$EDITOR "/Library/Application Support/runny/config.yaml"
cp runner-app.pem "/Library/Application Support/runny/"
```

(visible in `logs/launchd.err.log` under the home, or via `runnyctl doctor`);
it comes up on the next restart once the config lands. Either way, there is no
GUI prompt — a launchd-started daemon of any uid is auto-allowed Local Network
access. Once installed, apply any further config change with `runnyctl
edit-config` (below) — never by hand-editing `config.yaml` and restarting.

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

### Windows

Get `runnyd`/`runnyctl` onto the host first (ADR-0010): download
`runny_<version>_windows_{amd64,arm64}.zip` from the
[Releases](https://github.com/bojanrajkovic/runny/releases) page and extract
it anywhere on `PATH`.

`runnyctl install-daemon`/`uninstall-daemon` are the same commands on Windows,
run from an **elevated** prompt instead of via `sudo`. `--config` behaves the
same; `--operator` does not — it must name the account running the elevated
command (or be omitted), since staging writes the config/key using that
process's own access, and the home's ACL grants only the service account and
the named operator. A different `--operator` fails immediately with
`AccessDenied` rather than being silently accepted. The daemon runs under the
Windows Service Control Manager as a service named `runnyd` (`sc.exe query
runnyd`), using a virtual service account (`NT SERVICE\runnyd`) in place of
darwin's dedicated `_runny` account — nothing to create or reuse, its SID
derives deterministically from the service name. The home is
`C:\ProgramData\runny`, with an ACL granting the service account and your
operator account Modify (Windows ownership confers no implicit access, unlike
POSIX owner bits) instead of darwin's inheriting ACL; logs land in
`C:\ProgramData\runny\logs\service.err.log`. A failed service is restarted by
the SCM's recovery policy, the same role KeepAlive plays on darwin.
`uninstall-daemon` stops the service, deletes it, and removes the home — unlike
darwin, nothing is kept for reinstall, since the service account has no uid to
keep stable.

A configured pool provisions Linux guests (arch matching the host's own —
`amd64` on the amd64 Windows hosts this has been validated on) via bare
Hyper-V compute systems (ADR-0026) — no `tart` binary, no classic Hyper-V VM. **A running guest
does not show up in `Get-VM` or Hyper-V Manager**: bare compute systems bypass
`vmms` entirely, so the operator-visible artifact is a `vmwp.exe` worker
process per guest (`Get-Process vmwp`), not a `VM` object. The Default Switch
must exist (Hyper-V's default install creates it) and have headroom in its
endpoint pool for concurrent slots.

## The Runny app and the command-line tool

The `Runny.app` bundle carries signed copies of `runnyd` and `runnyctl`. The app
is non-privileged (ADR-0023): it does **not** install `runnyctl` to a system path.
Get `runnyctl` on your PATH via the Homebrew tap formula, the Homebrew cask
(`brew install --cask bojanrajkovic/tap/runny-app`), or run it from inside the bundle
(`/Applications/Runny.app/Contents/MacOS/runnyctl`) — it is the same build as the
bundled daemon.

The app **installs and manages the daemon** as a per-user LaunchAgent —
the desktop-GUI install channel. From a copy of Runny in `/Applications`:

- **Settings → Daemon → "Start runnyd at login"** registers the bundled `runnyd`
  via `SMAppService` (one confirmation names the launchd label). The first guest
  boot raises the **Local Network** prompt; the app surfaces a grant card
  proactively if the grant is missing or pending, *before* a guest dial fails (the
  per-user agent is subject to that one-time prompt — see "Why this is not just
  `launchctl load`" above).
- A **Start** affordance appears in the menu bar and main window when the agent is
  installed but the daemon isn't running.
- After upgrading the app to a newer build, **"Update Daemon"** drains running
  jobs, then restarts onto the freshly-bundled binary (a `runnyctl reload --wait`
  by another name).
- Toggling it off uninstalls the agent; mid-job it warns that the running job is
  abandoned.

Install requires Runny in `/Applications` (a translocated or `~/Downloads` launch
is refused, recoverably). **The two channels split by audience:** the app is the
**desktop-GUI** install path (a per-user agent in your login session); `sudo
runnyctl install-daemon` is the **headless-fleet** path (a non-root system
LaunchDaemon). The app installs its per-user agent only when no system daemon is
present. If a system LaunchDaemon is already installed, the app observes it (status
streams normally as a sibling client over the same socket) and shows an observer
banner pointing at `runnyctl uninstall-daemon` instead of offering the install
toggle. The app is non-privileged — it never installs, updates, or removes the
system daemon (that is `runnyctl`'s job, raising no admin prompt; see ADR-0023).
A hand-run or leftover runnyd converges via the single-instance flock and does not
require manual cleanup. A bundled `runnyctl` on PATH can lag a `brew`-upgraded
`runnyd`; when it does, `runnyctl` prints a one-line version-skew warning to stderr
before its output (warn, never refuse).

## Applying config changes

runnyd reads its `config.yaml` once, at startup. The canonical way to change it
— desktop or headless — is `runnyctl edit-config`: `visudo` semantics for the
resolved home's config.

```sh
runnyctl edit-config
```

It opens the resolved home's `config.yaml` (system home if one exists, else
`~/.runny`) in `$VISUAL`/`$EDITOR` (falling back to `vi`), seeding a fresh
skeleton if none exists yet. On save it runs `runnyd -test-config` against your
edit: an **error** reopens the editor with your changes intact (nothing is ever
discarded over a typo); a **warning** asks to confirm before applying; **ok**
proceeds straight through. Once validated it atomically replaces the on-disk
file and, if the daemon is running, drives the same validated reload as
`runnyctl reload` below (if it isn't running yet — e.g. a headless home just
staged by `install-daemon --config`'s next config edit — it reports "applies on
next start"). No `sudo` is needed for a system daemon's home either: the
operator reaches it through the install-time inheriting ACL, not ownership.

Editing the file directly (any editor, not through `edit-config`) still works,
followed by an explicit apply:

```sh
# edit config.yaml, then:
runnyctl reload --reason "why"
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

The blunt fallback remains: `runnyctl pause` every slot, wait until each sits
paused in BACKOFF (`runnyctl status`), then stop and let KeepAlive respawn —
`sudo launchctl bootout system/com.coderinserepeat.runnyd` for a system daemon, or
toggle the daemon off and back on in the Runny app for a per-user agent. Same
effect, no validation.

## Upgrading the daemon binary (headless)

A `brew upgrade` (or any re-install) updates the on-disk `runnyd`, but the
**running** daemon is still the old binary until something drains and respawns
it. On a headless host there's no GUI "Update Daemon" button, so the CLI does it:

```sh
brew upgrade runny        # delivers the new binary
sudo runnyctl upgrade-daemon
```

`upgrade-daemon` gates the update on the **new** binary validating the in-place
config — it execs the on-disk `runnyd -test-config ~/.runny/config.yaml` (local
checks only, no network) and reads the JSON verdict:

- **OK** → it issues the drain-gated reload (running jobs finish first, then
  launchd cold-starts the new binary), the same path as `runnyctl reload --wait`.
- **Warn** → it prints the warnings and refuses unless you pass `-force`; the
  upgrade still works, the config just has a soft footgun (e.g. an aggregate
  resource overcommit or a too-short deadline).
- **Error** → it prints the incompatibility and refuses; `-force` does **not**
  override a hard error. This is the case the gate exists for: a schema change
  the new binary rejects is blocked here instead of crash-looping the respawn
  under launchd KeepAlive.

`upgrade-daemon` uses a dedicated `UpgradeReload` RPC rather than the plain
`Reload`. This lets the running daemon accept a **forward-only config edit** — a
new or renamed key the new `runnyd` accepts but the old strict parser rejects.
When the running binary's own parser refuses the file, `upgrade-daemon` asks the
respawn target (the binary launchd would actually exec, re-resolved from the
plist symlink now) whether it accepts the config; if the target accepts, the
drain proceeds. If the symlink still points at the old binary (i.e. `brew
upgrade` ran but the symlink wasn't updated), the target also refuses and the
upgrade is blocked — no crash-loop is possible. A pre-feature running daemon
returns `Unimplemented`, which `upgrade-daemon` surfaces as a clear message to
run `runnyctl reload --wait` instead.

The daemon never self-upgrades — the restart is launchd's, triggered by the
operator. brew owns the binary delivery, so `upgrade-daemon` only validates and
reloads — there is no binary re-stage step.

You don't have to remember to check: when the running daemon's version lags the
installed `runnyctl` (the state a `brew upgrade` leaves until you reload), every
`runnyctl` command — including `runnyctl doctor` — prints a one-line hint first:
`a newer runnyd is available … — run runnyctl upgrade-daemon`. It's the existing
version-skew warning, pointed at the actionable verb in the daemon-behind
direction (a daemon *ahead* of the CLI still reads as "upgrade the lagging
install" instead).

## Troubleshooting: Local Network permission

Symptom: every cycle dies at `AWAIT_SSH` with `connect: no route to host`,
and the daemon never provisions a VM. Confirm with `runnyctl doctor` while a
guest is booting — it must show `local-network ok`. (Use `runnyctl doctor`,
which asks the *running daemon*; `runnyd -doctor` probes from your shell,
which over SSH inherits sshd's exemption and will say `ok` even when the
daemon is denied.)

If `local-network` is not ok:

1. Open **System Settings → Privacy & Security → Local Network**. If `runnyd`
   is listed, enable it, then restart the daemon: `sudo launchctl bootout
   system/com.coderinserepeat.runnyd` for a system daemon (KeepAlive respawns
   it), or toggle the daemon off and back on in the Runny app's Settings pane
   for a per-user agent.
2. If it is not listed, the daemon has never run where macOS could ask. For a
   per-user agent, the Runny app surfaces a grant card proactively when the
   grant is missing — open the app from a terminal **in the GUI session** (not
   over SSH, not inside tmux/screen) so the prompt can render, and accept the
   **"runnyd would like to find and connect to devices on your local network"**
   dialog. For a system daemon the grant is automatic (a launchd-started daemon
   of any uid is auto-allowed); if `local-network` is failing under a system
   daemon, revisit whether the daemon is actually running as `_runny` under
   launchd (`runnyctl doctor` and `sudo launchctl list com.coderinserepeat.runnyd`).
3. To isolate the permission from other network problems: run `runnyd` in the
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
runnyd reads config once at startup, so a recycle alone keeps the old setting,
and a hard restart kills in-flight jobs),
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

Stop runnyd (toggle the daemon off in the Runny app, or `sudo runnyctl
uninstall-daemon` for a system install), then re-bootstrap the old manager's
plist. runnyd leaves no durable state that interferes — `~/.runny/vms` is swept
on every start, and JIT runner registrations self-remove or are swept on the
next cold start.
