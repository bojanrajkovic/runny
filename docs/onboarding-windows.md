# Getting started with runny on Windows

This walks you from an empty Windows host to a runner picking up its first job:
create the GitHub App, install runny, write a config, register the service, and
(optionally) turn on telemetry. It is the guided happy path; once you are
running, [docs/deploy.md](deploy.md) is the reference for everything this skips
— reload, upgrades, the Chocolatey upgrade hook, and troubleshooting.

You need **Windows on amd64** (build 17763+) with **Hyper-V enabled**, and an
**elevated prompt** for the install steps. `amd64` is the validated
architecture; `windows/arm64` is accepted by the config loader on the same
"real but currently unvalidated" footing as `linux/arm64`.

**On a macOS host?** [docs/onboarding.md](onboarding.md) is that path — it
covers the Homebrew tap, the `Runny.app` desktop shape, and the Local Network
permission prompt, none of which exist here.

## What is different here

Two things, before any commands.

**There is one shape, not two.** The macOS guide opens by choosing between a
desktop app and a headless daemon. Windows has only the headless shape: the
Runny app is a SwiftUI target that exists on darwin only. You get `runnyd` and
`runnyctl`, the daemon runs under the Service Control Manager as a service
named `runnyd`, and its home is `C:\ProgramData\runny`. Nothing prompts, and
nothing needs a login session.

**Choose the guest OS you actually want, because the images are not equally
ready:**

| | **Linux guests** | **Windows guests** |
|---|---|---|
| Image | a published one works today (`ghcr.io/cirruslabs/ubuntu-runner-amd64`) | **you supply it** — see below |
| `os:` in the config | `linux` | `windows` |
| Default labels | `[self-hosted, Linux, X64]` | `[self-hosted, Windows, X64]` |
| `guest_env` / `guest_setup` | supported | **rejected at config load** |

A Windows guest image is not a stock Windows install. The runner is started by
a launcher baked *into the image* — a scheduled task in an AutoLogon desktop
session that polls `C:\actions-runner\.jitconfig`, starts the Actions runner
when it appears, redirects output to `C:\runny\runner.log`, and writes the
runner's exit code to `C:\runny\runner-exit.txt`. runnyd delivers the JIT
config and watches those two files; it never launches the runner itself.
[docs/architecture/runnyd.md](architecture/runnyd.md)'s "Windows guests"
section is the contract an image has to satisfy. If you are starting from
Linux guests, you can skip that entirely — start there, and come back.

Both guest kinds boot as **bare Hyper-V compute systems** (ADR-0026), not
classic Hyper-V VMs. The practical consequence is worth knowing before you go
looking for a guest that is running fine: **a running guest does not appear in
`Get-VM` or Hyper-V Manager.** Bare compute systems bypass `vmms` entirely, so
the operator-visible artifact is one `vmwp.exe` worker process per guest
(`Get-Process vmwp`). `runnyctl status` is the real answer to "what is
running".

## 0. Enable Hyper-V

From an elevated PowerShell prompt, if it is not already on:

```powershell
Enable-WindowsOptionalFeature -Online -FeatureName Microsoft-Hyper-V -All
```

This reboots. Afterwards, confirm the **Default Switch** exists — Hyper-V's own
install creates it, and runny attaches every guest's HNS endpoint to it:

```powershell
Get-VMSwitch -Name 'Default Switch'
```

It also needs headroom in its endpoint pool for as many slots as you plan to
run concurrently.

## 1. Create the GitHub App

Identical on every platform, so it lives in one place: follow
[docs/onboarding.md § 1](onboarding.md#1-create-the-github-app) and come back
with the **App ID** and the downloaded `.pem`. The short version is one App,
**Self-hosted runners: Read and write** and nothing else, webhooks off,
installed on the org or the exact repositories you will target.

## 2. Install runny

From an elevated prompt:

```powershell
choco source add -n=runny -s=https://bojanrajkovic.github.io/choco-feed/index.json
choco install runny
```

Chocolatey **2.4.1 or newer** is required — earlier versions cannot resolve an
explicit `--version` against a v3-only feed. The feed is a static NuGet v3 feed
served from GitHub Pages, the counterpart to the macOS Homebrew tap, with no
server behind it (ADR-0010). `choco install runny --pre` takes pre-release
builds.

This installs the `runnyd.exe` and `runnyctl.exe` binaries and registers no
service — that is step 4. There is **no winget package**; it was tried and
dropped (ADR-0010). If you would rather not use Chocolatey at all, extract
`runny_<version>_windows_amd64.zip` from the
[Releases](https://github.com/bojanrajkovic/runny/releases) page anywhere on
`PATH` — you lose the upgrade hook that stops the service before swapping its
binary, so read [docs/deploy.md](deploy.md#windows) before your first upgrade.

## 3. Write the config

runny reads one file: `config.yaml`. Write it **anywhere** for now — it is a
one-shot seed that `install-daemon --config` stages into
`C:\ProgramData\runny` for you in step 4, key and all. Do not hand-place it in
the home; a system install's config is daemon-owned, and `runnyctl edit-config`
is how you change it afterwards.

A minimal config, one org-scoped Linux pool on this Windows host:

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/bojanrajkovic/runny/main/tools/configschema/config.schema.json
pools:
  - name: linux
    os: linux
    image: ghcr.io/cirruslabs/ubuntu-runner-amd64:latest
    count: 2
    target:
      org: my-org
    github:
      app_id: 123456
      private_key_path: C:\Users\you\Downloads\runner-app.pem
```

For Windows guests instead, `os: windows` and your own image reference — and
note that `guest_env` and `guest_setup` are **refused at load** for a windows
pool, because the runner launches through the image's own launcher rather than
an injectable shell script. Bake what they would have set into the image.

`private_key_path` names where the `.pem` from step 1 lives **right now**. It
is read verbatim, so it must be an absolute path — and on this platform that
means a drive-qualified one. `install-daemon --config` copies the key into the
home and rewrites this path to point there; you do not hand-place it.

Everything else takes a default. The ones worth knowing are the same across
platforms and are listed in
[docs/onboarding.md § 3](onboarding.md#3-write-the-config) — `target`, `count`,
`labels`, `ssh_user`/`ssh_password`, `cpu_cores`/`ram_gb`. Two notes specific
to here: the two-concurrent-guest cap in that list is Apple's
Virtualization.framework limit and does not apply to a Hyper-V host, and
`cpu_cores`/`ram_gb` matter more than they do on macOS, because a guest that
sets neither silently runs on whatever the image baked (commonly a
conservative 2 cores / 4 GiB).

## 4. Register the service

From an **elevated** prompt — this is where `sudo` appears in the macOS guide:

```powershell
runnyctl install-daemon --config .\config.yaml
```

This copies the `.pem` your config points at into the home, rewrites the
config's copy of that path, validates the result with `runnyd -test-config`,
and only then starts the service — so a typo fails right here rather than as a
crash-loop.

`--operator` behaves differently than on macOS: it must name **the account
running this elevated command**, or be omitted. Staging writes the config and
key using this process's own access, so a different `--operator` fails
immediately with `AccessDenied` rather than being silently accepted.

What you get:

- A service named `runnyd` (`sc.exe query runnyd`), running as the virtual
  service account `NT SERVICE\runnyd` — nothing to create, its SID derives
  deterministically from the service name. There is no `_runny`-style local
  account as there is on macOS.
- The home at `C:\ProgramData\runny`, with an ACL granting the service account
  and your operator account Modify. Windows ownership confers no implicit
  access, unlike POSIX owner bits, so the ACL is the whole story. Both entries
  inherit, and nothing below the home is protected — so a revoke against the
  home reaches every artifact under it.
- Logs at `C:\ProgramData\runny\logs\service.err.log`.
- SCM recovery configured to restart a failed service, the role `KeepAlive`
  plays on darwin.

## 5. Verify

```powershell
runnyctl doctor    # every check green, including runner-perm
runnyctl status    # slots cycling: pull -> boot -> provision -> JIT-register -> LISTENING
```

When a slot reaches **LISTENING**, its runner shows **online** in your org or
repo's runner list, and the next matching job runs on it. Ask any failure
`runnyctl why <slot>`.

If a slot is stuck and you want to know whether a guest is alive at all,
remember `Get-VM` will not tell you — use `Get-Process vmwp` for the worker
processes and `runnyctl status` for the daemon's own view. To get inside a
guest, `runnyctl debug <slot>` injects your public key and holds the slot in
DEBUG so you can SSH in yourself.

## 6. Turn on telemetry (optional)

Platform-independent, and unchanged from the macOS guide: telemetry is off
until an `observability.otlp.endpoint` exists, and that one value turns on both
traces and metrics. See
[docs/onboarding.md § 5](onboarding.md#5-turn-on-telemetry-optional) for the
config block and
[docs/deploy.md](deploy.md#enabling-otlp-telemetry) for collector caveats and
query recipes.

## Where to go next

- **Upgrades** → [docs/deploy.md](deploy.md#windows). `choco upgrade runny`
  handles the service for you, because Windows locks the image file of a
  running process. `runnyctl upgrade-daemon` is darwin-only and refuses here.
- **Editing config, troubleshooting, migration** → [docs/deploy.md](deploy.md).
  Apply an edit with `runnyctl edit-config` or `runnyctl reload`, never by
  killing the service.
- **How it works** → [docs/architecture/](architecture/), and
  [docs/architecture/runnyd.md](architecture/runnyd.md)'s "Windows guests"
  section for the launcher hand-off.
- **Why it works that way** → [docs/architecture-decisions/](architecture-decisions/),
  ADR-0026 for the Hyper-V/HCS backend.
- **Security posture** → [docs/security.md](security.md).
