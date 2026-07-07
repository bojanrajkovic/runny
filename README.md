<img src="apps/Runny/Resources/AppIcon.xcassets/AppIcon.appiconset/icon_128.png" width="128" align="right">

# runny

[![codecov](https://codecov.io/gh/bojanrajkovic/runny/branch/main/graph/badge.svg)](https://codecov.io/gh/bojanrajkovic/runny)

An observable macOS GitHub Actions runner daemon: ephemeral single-use VMs on
Apple's Virtualization.framework, fully compatible with
[tart](https://github.com/cirruslabs/tart)'s bundle and OCI image format — with
no tart binary at runtime.

**Design rule: no operation is ever unbounded, and no failure is ever silent.**
Built because the predecessor converted every transient failure into a permanent
silent outage.

## Install

Requires **macOS Sequoia (15.0+)** on **Apple Silicon** and a GitHub App with
the runner-administration permission. **New here? Start with
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
pull image → clonefile → boot (in-process via
[vz](https://github.com/Code-Hex/vz)) → SSH provision → JIT-register → run one
job → destroy → repeat. Every failure converges to destroy-and-recycle with
capped backoff; every cycle writes a machine-readable post-mortem.

**`runnyctl`** — the operator CLI over a unix socket: live status and runner
logs, recycle/pause slots, per-cycle post-mortems (`why`), environment checks
(`doctor`), version-gated daemon upgrades (`upgrade-daemon`).

**`Runny`** — a SwiftUI menu-bar app: slot health at a glance, live runner logs,
daemon lifecycle management, and in-app update notifications when a new release
lands in the Homebrew tap.

## Building from source

```sh
mise install
bazel build //...
```

Pure-Go packages build and test anywhere; the daemon binary and Runny.app
require macOS arm64. See [CONTRIBUTING.md](CONTRIBUTING.md) for the full dev
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
