# Contributing to runny

The human-developer workflow. Agent-facing guidance and the project-wide index live in `CLAUDE.md`; the rules that govern the documentation system live in `docs/documentation-system.md`.

## Setup

- **Toolchain:** mise-managed (`mise install`): Bazel, Go, Node (commitlint only), lefthook — `.mise.toml` is the single home for every tool version, including Bazel's.
- `npm install` once for the commitlint dev dependency, then `lefthook install` to wire the git hooks.
- **macOS hosts (ix):** Command Line Tools suffice for the daemon (cgo + Virtualization.framework, verified); full Xcode is required to build the Runny app (`apps/Runny`) — and therefore for the pre-push hook's `bazel build //...` on a macOS host (rules_apple needs the SDK; Xcode is never opened — ADR-0007). On a CLT-only macOS host, daemon-only work can still build the tree minus the app: `bazel build -- //... -//apps/... -//proto/runny/v1:runnyv1_swift_proto`.
- **Windows hosts:** `mise install` covers the toolchain here too — bazel included, via mise's aqua backend, which supports windows/amd64 for the pinned version. Clone with `git config --global core.autocrlf false` set **first** (see "Cross-host dev loops"), and with git-lfs available: `internal/vhdx/testdata`'s binary VHDX fixtures are LFS-tracked. Nothing about the Runny app builds here.

## Commands

| Command | Description |
| --- | --- |
| `bazel build //...` | Build everything buildable on this host |
| `bazel test //...` | Run the test suite |
| `bazel run //tools/format` | Format the tree (gofumpt, buildifier, SwiftFormat — the nicklockwood tool, not apple/swift-format) |
| `bazel run //:gazelle` | Regenerate BUILD files after import changes |
| `bazel run //tools/configschema -- -write` | Regenerate `config.schema.json` after changing the `home.Config` struct |

**Config schema:** `tools/configschema/config.schema.json` is generated from `home.Config` and committed (editors reference it; see docs/deploy.md). After any change to the config struct, regenerate it with the command above — a golden test (`//tools/configschema:configschema_test`) fails the build until the committed file matches the struct, the same no-silent-drift discipline as the `config-drift` doctor check.

**Dependency workflow:** `go mod tidy -e` → `bazel run //:gazelle` → `bazel mod tidy`. Add Go deps at latest stable; Renovate keeps pins current.

**Known wrinkle (the `-e` and the ordering are load-bearing):** imports of `proto/runny/v1` resolve only in-graph (ADR-0006), so plain `go mod tidy` *aborts mid-attribution* and leaves every dep marked `// indirect` — which makes `bazel mod tidy` strip them from `use_repo` and breaks the build. `-e` tolerates the unresolvable import and attributes correctly. gazelle must run before `bazel mod tidy` so new BUILD references exist when use_repo is recomputed. gopls likewise can't resolve the generated import.

**Renovate and the bzlmod lockfile:** hosted Renovate bumps `MODULE.bazel` (and the bazel version in `.mise.toml`) but cannot run bazel, so `MODULE.bazel.lock` arrives stale and CI's `--lockfile_mode=error` refuses the PR. That refusal is the guard working. The `renovate-lockfile` workflow regenerates and pushes the lockfile to Renovate's PR branches automatically (bazel-version bumps included — new versions resolve different implicit deps). If it misfires, the manual flow still works: check out the branch, `bazel mod deps --lockfile_mode=update`, build/test, commit the lockfile, merge once CI re-proves it. Go-dep PRs never need this — gazelle's `go_deps` extension is reproducible and deliberately unrecorded in the lockfile.

**Renovate and go.sum:** every `gomod`-manager Renovate PR arrives with a bumped `go.mod` but a stale `go.sum`. Renovate's artifact updater runs `go get -t ./...` before `go mod tidy` to re-resolve the whole graph, and that step hits the same in-graph-only `proto/runny/v1` import above — but `go get` lost its own `-e` tolerance flag when Go moved to module mode, so `postUpdateOptions: ['gomodTidyE']` (which only reaches the later `go mod tidy` call) can't save it. This is not a misconfiguration; there is no Renovate config that avoids it. The `renovate-gosum` workflow regenerates `go.sum` (and gazelle/`bazel mod tidy`, for the rarer bump that adds or removes a package) and pushes it back automatically. If it misfires, the manual flow is the same as above: check out the branch, run the three commands, build/test, commit, merge once CI re-proves it.

## Cross-host dev loops

runny targets two host platforms, and **neither one can test the other**. Primary development happens on the Linux box, where everything pure-Go builds and tests; both platform-gated halves need a real host of their own.

| Gated on | Targets | Needs |
| --- | --- | --- |
| darwin | `internal/vm`'s vz cgo code, `cmd/runnyd`'s final binary, the Runny app | macOS arm64 host |
| windows | `internal/winhcs`, `internal/vm`'s hcs/neighbortable/netfixup/reap files, `internal/vhdx`, the SCM installer, `cmd/runnyd`'s svc/lock/platform files | Windows amd64 host |

The windows half is pure Go — no cgo — which makes it easy to assume a macOS `bazel test //...` covers it. It does not compile a line of it. Both halves are covered in CI (the `macos-26` and `windows-2022` lanes), so a PR is gated either way; the loops below are for finding out before you push.

### Linux ↔ macOS (ix)

```
rsync -a --exclude bazel-\* --exclude node_modules . brajkovic@ix:~/src/runny/
ssh brajkovic@ix 'cd ~/src/runny && bazel test //...'
```

The daemon binary must be codesigned with the `com.apple.security.virtualization` entitlement to boot VMs (ad-hoc signing is fine locally; see ADR-0008):

```
codesign -s - --entitlements tools/sign/runnyd.entitlements --force bazel-bin/cmd/runnyd/runnyd_/runnyd
```

### → Windows

**Compile-check locally first — it catches most of it and needs no second host.** A cross-build proves the windows-gated tree still compiles, including the in-graph proto package a plain `GOOS=windows go build` can't resolve:

```
bazel build --platforms=@rules_go//go/toolchain:windows_amd64 -- \
  //cmd/runnyd //cmd/runnyctl //internal/winhcs/... \
  -//internal/winhcs/log:log_test -//internal/winhcs/oc:oc_test
```

The two excluded test targets are not optional: cross-compiling a windows `go_test` needs a windows test-execution platform this host doesn't have. This is the same command CI's Linux lane runs.

**Windows-only `_test.go` files are the blind spot in that check** — Bazel can't cross-build them, so a test file that doesn't compile passes the cross-build and fails only on the Windows lane. For a package that doesn't import the generated proto, `go vet` type-checks the test sources without Bazel:

```
GOOS=windows go vet ./internal/opacl/
```

**Executing the suite needs a real Windows host.** Get the tree there (a branch push and pull is the least surprising way — there is no rsync on a stock Windows OpenSSH host), then:

```
bazel --output_base=C:/b test --config=ci --test_env=USERPROFILE -- //... -//apps/Runny/...
```

Four things in that line are load-bearing, each earned:

- **`--output_base=C:/b`** — Bazel's default per-workspace-hash path under `%USERPROFILE%` nests deep enough to hit `MAX_PATH`. Forward slashes, not `C:\b`: in Git Bash an unquoted `\b` loses its backslash before Bazel sees it, and Bazel accepts forward-slash paths on Windows natively.
- **`--test_env=USERPROFILE`** — Bazel's local test sandbox doesn't pass `USERPROFILE` through by default on Windows the way it does `HOME` on unix, and `os.UserHomeDir` needs it.
- **`-//apps/Runny/...`** — a SwiftUI target that cannot build off darwin.
- **`core.autocrlf false`, set before cloning** — mangled line endings in checked-out fixtures are a one-way trip, not something to fix up afterwards.

**The pre-push hook does not work as-is on a Windows host.** It runs bare `bazel build //...` / `bazel test //...`, which includes `//apps/Runny/...` and so fails before reaching anything real — the same wrinkle a CLT-only macOS host has under Setup, without that host's option of installing the SDK. Run the excluded form above by hand.

CI never boots guests on either platform (GitHub's macOS runners are VMs themselves, and the Windows runners have no nested-virtualization guarantee), so anything that actually starts a compute system is verified on a real host, by hand.

## Code conventions

- **gofumpt formatting and nogo analysis** ride the build; don't fight them.
- **Errors:** wrap with `%w` and context at each boundary; sentinel errors as package-level `var Err...`. No panics outside `main` initialization — a daemon that panics drops its VMs.
- **Deadlines:** any function that touches a guest, the network, or a subprocess takes a `context.Context` and honors it. SSH clients are constructed only by `internal/sshx` (ADR-0002).
- **Build constraints:** vz-touching code is `//go:build darwin` with interface seams so the FSM and everything above it tests on Linux.
- **Logging:** structured (`log/slog`) to the daemon's file sink + ring buffer; never `fmt.Print*` in the daemon.

## Commits and pull requests

- **Conventional Commits**, validated by the commit-msg hook (`@commitlint/config-conventional`); the type enum is owned by `commitlint.config.mjs`. Wrap body lines at 100 characters.
- **Atomic commits** — one logical change each, describable in a sentence without "and".
- **Hooks (lefthook):** pre-commit formats staged files (restaging fixes) and zizmor-audits staged workflow files (offline); commit-msg runs commitlint; pre-push runs `bazel build` + `bazel test`. Do not bypass them.
- **Squash-merge** — the PR title becomes the shipped subject (gated by `pr-title.yml`); the PR body becomes the commit body, so write it as "what shipped". Split multi-phase work into multiple PRs, each one logical change.
- **`main` is protected by the "Protect Main" ruleset** — PR-only with required checks (strict), code-owner review, signed commits, linear history, no force-push or deletion. Repo admins and Renovate bypass; read the ruleset itself for the authoritative rule list.

## CI

GitHub Actions gate every PR and push to `main`: `ci.yml` (Linux: build/test/format, plus the windows/amd64 cross-build; macOS: darwin targets + an ad-hoc-signed `runnyd` artifact; Windows: the suite *executed* on `windows-2022`, which is what the Linux lane's cross-build can't do; race: `-race` rebuild; zizmor: workflow audit; coverage: combined Go+Swift lcov uploaded to Codecov, informational only per `codecov.yml` — no status check blocks) and `pr-title.yml`. All checks must pass before merge. CI never boots guests — GitHub's macOS runners are VMs themselves — so VM-touching verification happens on a real host.

The Windows lane excludes only `//apps/Runny/...`. In particular `//internal/sysdaemon` is **not** excluded: it once was, on the grounds that the package was macOS-LaunchDaemon-only, which stopped being true when the SCM installer landed — leaving the one package that installs the daemon as a Windows service with no CI executing its tests on Windows at all. Sixteen tests had never run. If you exclude a target from a platform lane, the justification has to stay true as the target changes.

### CI security

The posture these workflows implement — SHA-pinned actions, cold cache-isolated artifact builds, least-privilege tokens, environment-scoped release secrets, zizmor auditing — is the canonical subject of [`docs/security.md`](docs/security.md). When you touch CI, preserve it:

- **Pin every new action by commit SHA** (the tag in a trailing comment), never by a movable tag; Renovate updates the pins.
- **Keep caches out of the `artifact` job** — Bazel/tool caches are restored only in build/test jobs so a poisoned cache cannot reach a deployable binary. Preserve this when adding release workflows.
- **`persist-credentials: false`** on every checkout; declare per-job permissions, not workflow-level.
- **Fix zizmor findings rather than suppress them.** The hook audits staged workflows offline; the CI job runs zizmor-action (auditor persona) online and fails on any finding (it also uploads SARIF to the Security tab — a dismissal there does not unblock the gate). A real suppression needs an inline `# zizmor: ignore[rule]` whose justification survives review.
- **GitHub App tokens (`create-github-app-token`) always declare `permission-*` inputs** — an unscoped token inherits every permission the App installation has. Jobs that mint App tokens declare `environment: releaser-app`; jobs that sign artifacts declare `environment: release`. No job needs both.

### Codesigning tiers

- **Ad-hoc (current)**: CI signs `runnyd` with `codesign -s -` plus the virtualization entitlement; the artifact boots VMs on any host. No setup required.
- **Developer ID (when distribution matters)**: requires an Apple Developer Program membership. Export the Developer ID Application certificate as `.p12` and provision it (plus the App Store Connect notary key) onto the `release` environment via `tools/setup-secrets.sh`. The signing workflow upgrade lands once those secrets exist.

**Clean-machine Gatekeeper check before a release tag.** The `.app` now carries `runnyd`/`runnyctl` as nested Mach-Os, signed inside-out. `bazel test`'s `bundle_contents_test` proves each is validly signed, but on the build host that only attests the local/ad-hoc trust — it cannot prove Apple *notarized the nested binaries* so Gatekeeper accepts them offline. Before tagging a release, verify the notarized `.dmg` on a clean machine (or a fresh VM with networking off, forcing offline ticket validation): `spctl -a -vv Runny.app` must report `accepted` / `source=Notarized Developer ID`, and launching it must not be blocked. This is the one gate the build host can't run; skipping it ships a bundle that passes CI yet Gatekeeper-rejects on a user's Mac.

On that same clean machine — at the **minimum supported macOS** and in a **GUI login session** (the prompt only renders there) — also confirm the app-managed daemon path: install the agent, boot a guest, and verify the **Local Network prompt names `runnyd`** and the grant sticks. The daemon embeds no `NSLocalNetworkUsageDescription` because a signed bare per-user LaunchAgent raises the prompt on its own (verified on a later macOS; this gate re-confirms it on the floor version, where Local Network TCC first shipped). The grant must also **survive a version upgrade** (re-launch an upgraded build against an existing grant → no re-prompt), which holds only while the bundled `runnyd`'s signing identity stays stable across releases.

## Boundaries

- `bazel-*` symlinks and `node_modules/` are generated; never edit them.
- `MODULE.bazel.lock` is generated by Bazel; do not hand-edit.
- `.env` / `user.bazelrc` hold local settings or secrets; never commit them.
