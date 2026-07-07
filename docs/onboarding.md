# Getting started with runny

This walks you from an empty macOS host to a runner picking up its first job:
create the GitHub App, install runny, write a config, start the daemon, and
(optionally) turn on telemetry. It is the guided happy path; once you are
running, [docs/deploy.md](deploy.md) is the reference for everything this skips
— reload, upgrades, migration, and troubleshooting.

You need **macOS Sequoia (15.0+)** on **Apple Silicon**. Everything else you
install below.

## Pick a shape first

One choice runs through the whole guide. runny installs in one of two shapes,
split by audience:

| | **Desktop** | **Headless** |
|---|---|---|
| For | your own Mac, with a login session | a fleet host, no one logged in |
| You get | `Runny.app` — menu-bar status, live logs, daemon toggle | `runnyd` + `runnyctl` on your `PATH` |
| Runs as | a per-user LaunchAgent | a non-root system LaunchDaemon |
| Config lives in | `~/.runny/` | `/Library/Application Support/runny/` |
| Local Network grant | one-time prompt, first guest boot | automatic |

The two are mutually exclusive — pick the row that matches your host and follow
that column throughout. The config file and its contents are identical either
way; only its location differs.

## 1. Create the GitHub App

runny registers each runner with a short-lived, just-in-time config minted
through a GitHub App (App JWT → installation token → JIT config). Create the App
once, install it on your org or repo, and hand runny the App ID and private
key.

1. Go to **Settings → Developer settings → GitHub Apps → New GitHub App** (org
   settings for an org-scoped runner, your account for a personal repo).
2. Give it a name and any homepage URL. Uncheck **Webhook → Active** — runny
   polls, it receives nothing.
3. Under **Permissions**, grant **Self-hosted runners: Read and write** (this is
   the `administration: write` scope runny asserts on every minted token) and
   nothing else.
4. Create the App, then **Generate a private key** and download the `.pem`.
5. **Install** the App (left sidebar → Install App) on the org, or on the exact
   repositories you will target.

Keep the **App ID** (shown on the App's settings page) and the downloaded
`.pem`. You will reference both from the config in step 3. A pool whose token
lacks `administration: write` fails loudly at `runnyd -doctor` — a permission
upgrade queues per-installation approval, so grant it before you install.

## 2. Install runny

Both shapes install from the Homebrew tap.

**Desktop:**

```sh
brew install --cask bojanrajkovic/tap/runny-app
```

Or drag `Runny.app` from the `.dmg` on the
[Releases](https://github.com/bojanrajkovic/runny/releases) page into
`/Applications`. (The cask auto-updates; a dragged `.dmg` shows an in-app "update
available" banner instead.) You now have `Runny.app`; hold off on opening it
until the config is in place.

**Headless:**

```sh
brew install bojanrajkovic/tap/runny
```

This installs the `runnyd` and `runnyctl` binaries only — it registers no
service. You register the system daemon in step 4.

## 3. Write the config

runny reads one file: `config.yaml`, in the home for your shape (`~/.runny/` for
desktop, `/Library/Application Support/runny/` for headless). **Desktop users:**
create it directly at that path now. **Headless users:** write it anywhere for
now — it's a one-shot seed that `install-daemon --config` stages into the
system home for you in step 4, keys and all.

A minimal config is one pool pointed at one org:

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

`private_key_path` names where the `.pem` from step 1 lives **right now**
(`app_id` is your App's ID) — set it to wherever you actually put the file.
It's read verbatim (no `~` expansion), so it must be an absolute path.
**Headless:** this is just the staging source; `install-daemon --config`
copies the key into the system home and rewrites this path to point there —
you don't hand-place it. **Desktop:** this IS the final path, so put the file
there directly. Every key not shown takes a default; the ones worth knowing:

- **`target`** — either `org: <name>` **or** `owner: <user>` + `repo: <name>`,
  never both. This is the registration scope.
- **`count`** — slots in the pool, each an independent runner (default `1`).
  Apple's Virtualization.framework caps a host at **two** concurrent macOS
  guests, so the effective total ceiling across all `darwin` pools is 2.
- **`labels`** — the workflow `runs-on` labels (default `[self-hosted, macOS,
  ARM64]` for `darwin`).
- **`ssh_user` / `ssh_password`** — guest login (default `admin`/`admin`, which
  is what the cirruslabs images ship).
- **`cpu_cores` / `ram_gb`** — override the image's baked defaults (cirruslabs
  images ship a conservative 2 cores / 4 GiB).
- **`guest_env`** — a map of environment variables exported into the guest
  before the runner launches, so `run.sh` and every job step inherit them (e.g.
  `HTTPS_PROXY`/`NO_PROXY` to route job traffic through a host-side proxy). Keys
  must be valid environment-variable names; it is not for secrets (the values
  land in the guest's process args during provisioning).

The [`config.schema.json`](../tools/configschema/config.schema.json) referenced
in the modeline gives you autocomplete and inline validation in any
schema-aware editor. It checks the file's shape; the daemon's load-time
validation and `runnyd -doctor` remain authoritative for the semantic rules a
schema can't express. Deadlines, backoff, and retention all have sane defaults
— see [docs/deploy.md](deploy.md) for the full surface.

## 4. Start the daemon

**Desktop:** open `Runny.app` (it must be in `/Applications`) and toggle
**Settings → Daemon → "Start runnyd at login."** That registers the bundled
`runnyd` as your LaunchAgent. The **first guest boot** raises a one-time
**Local Network** prompt — accept
*"runnyd would like to find and connect to devices on your local network."* The
app surfaces a grant card if the permission is missing, so you will not miss it.

**Headless:** stage the config you just wrote and register the system daemon in
one step:

```sh
sudo runnyctl install-daemon --config ./config.yaml
```

This copies the `.pem` your config points at into the system home, rewrites
the config's copy of that path to the in-home location, validates the result
with `runnyd -test-config`, and only then starts the daemon — so a typo fails
right here, not as a crash-loop. (Skip `--config` and it registers immediately,
crash-looping until you hand-land a config — see
[docs/deploy.md](deploy.md#headless-system-daemon) if you need that path.) A
system daemon needs no Local Network prompt; launchd auto-allows it. Change the
config later with `runnyctl edit-config` — never by hand-editing `config.yaml`
and restarting.

**Verify, either shape:**

```sh
runnyctl doctor    # every check green — including runner-perm and, with a guest booting, local-network
runnyctl status    # slots cycling: pull → boot → provision → JIT-register → LISTENING
```

On the desktop, `runnyctl` comes from the cask or from inside the bundle
(`/Applications/Runny.app/Contents/MacOS/runnyctl`). When a slot reaches
**LISTENING**, its runner shows **online** in your org or repo's runner list,
and the next matching job runs on it. Watch one go end to end with `runnyctl
status`; ask any failure `runnyctl why <slot>`.

## 5. Turn on telemetry (optional)

Telemetry is **off by default** — with no `observability` block, runny opens no
OTEL SDK and sends nothing. Add the block to export traces and metrics over OTLP
to one collector:

```yaml
observability:
  otlp:
    endpoint: https://collector.example:4317   # https = TLS, http = insecure/local
    metrics_interval: 60s                        # optional; default 60s
    headers:                                     # optional; sent on every export
      x-honeycomb-team: ${env:HONEYCOMB_API_KEY}
```

`endpoint` is the single switch: a non-empty value turns on both signals. It
adds **outbound egress only** — the daemon still opens no listening socket; it
dials the collector the way it dials GitHub. `headers` carries backend auth
(every OTLP backend authenticates with one — `x-honeycomb-team`, `api-key`,
`authorization: Bearer …`), and values may reference environment variables with
the collector's `${env:VAR}` syntax, resolved once at load. An unset variable
refuses the config rather than exporting an empty credential.

Apply the change with `runnyctl reload --reason "enable telemetry"`. Traces
arrive as one span tree per runner cycle; metrics as `runny.*` instruments.
[docs/deploy.md](deploy.md#enabling-otlp-telemetry) has the collector-pipeline
caveats and a full set of PromQL query recipes;
[docs/architecture/observability.md](architecture/observability.md) explains the
design.

## Where to go next

- **Editing config after startup, upgrades, migration, troubleshooting** →
  [docs/deploy.md](deploy.md). runny reads config once at startup; apply an edit
  with `runnyctl reload`, never by killing the daemon.
- **How it works** → [docs/architecture/](architecture/).
- **Why it works that way** → [docs/architecture-decisions/](architecture-decisions/).
- **Security posture** → [docs/security.md](security.md).
