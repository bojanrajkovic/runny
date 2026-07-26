<img src="apps/Runny/Resources/AppIcon.xcassets/AppIcon.appiconset/icon_128.png" width="128" align="right">

# runny

[![codecov](https://codecov.io/gh/bojanrajkovic/runny/branch/main/graph/badge.svg)](https://codecov.io/gh/bojanrajkovic/runny)

An observable GitHub Actions runner daemon: ephemeral single-use VMs on Apple's
Virtualization.framework (macOS hosts, macOS and Linux guests) or bare Hyper-V
compute systems (Windows hosts, Linux and Windows guests), fully compatible with
[tart](https://github.com/cirruslabs/tart)'s bundle and OCI image format — with
no tart binary at runtime.

**Design rule: no operation is ever unbounded, and no failure is ever silent.**
Built because the predecessor converted every transient failure into a permanent
silent outage.

## Install

Requires a GitHub App with the runner-administration permission, and either
**macOS Sequoia (15.0+)** on **Apple Silicon** or **Windows** with Hyper-V
(build 17763+). The quick install below and the guided walkthrough are the macOS
path; for a Windows host go straight to
[docs/deploy.md](docs/deploy.md)'s "Windows" section, which covers the
Chocolatey feed and `runnyctl install-daemon`. **New here? Start with
[docs/onboarding.md](docs/onboarding.md)** — it walks you from an empty host to
a runner's first job: the GitHub App, install, config, daemon, and telemetry.
The quick install below is the short version; [docs/deploy.md](docs/deploy.md)
is the operator reference.

**Desktop — Runny.app (menu-bar status + daemon manager):**

```sh
brew install --cask bojanrajkovic/tap/runny-app
```

Open Runny.app and toggle Settings → Daemon → "Start runnyd at login" to
install the per-user LaunchAgent. The one-time Local Network prompt will appear
the first time a guest boots.

**Headless — non-root system LaunchDaemon:**

```sh
brew install bojanrajkovic/tap/runny
sudo runnyctl install-daemon
```

Place your config at `/Library/Application Support/runny/config.yaml` before
running `install-daemon`. See [docs/deploy.md](docs/deploy.md).

## What's in the box

**`runnyd`** — the daemon. One deadline-bounded state machine per runner slot:
pull image → clone → boot (in-process, via
[vz](https://github.com/Code-Hex/vz) on macOS or Hyper-V compute systems on
Windows) → SSH provision → JIT-register → run one job → destroy → repeat. Every failure converges to destroy-and-recycle with
capped backoff; every cycle writes a machine-readable post-mortem.

**`runnyctl`** — the operator CLI over a local control channel (a unix socket on
macOS, a named pipe on Windows): live status and runner
logs, recycle/pause slots, per-cycle post-mortems (`why`), environment checks
(`doctor`), version-gated daemon upgrades (`upgrade-daemon`).

**`Runny`** — a SwiftUI menu-bar app (macOS only): slot health at a glance, live runner logs,
daemon lifecycle management, and in-app update notifications when a new release
lands in the Homebrew tap.

## Building from source

```sh
mise install
bazel build //...
```

Platform-gated targets build only on their own platform: the vz backend and
Runny.app need macOS arm64, and the Hyper-V backend, the vendored HCS binding,
and the SCM installer need a Windows toolchain. Everything else builds and tests
anywhere. See [CONTRIBUTING.md](CONTRIBUTING.md) for the full dev
loop, codesigning tiers, and the CI setup.

## Docs

| Topic | |
|---|---|
| Getting started (zero to first job) | [docs/onboarding.md](docs/onboarding.md) |
| Install, config, GitHub App setup, migration | [docs/deploy.md](docs/deploy.md) |
| How it works | [docs/architecture/](docs/architecture/) |
| Why it works that way | [docs/architecture-decisions/](docs/architecture-decisions/) |
| Security posture | [docs/security.md](docs/security.md) |

## License

MIT — see [LICENSE](LICENSE).
