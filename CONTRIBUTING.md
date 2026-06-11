# Contributing to runny

The human-developer workflow. Agent-facing guidance and the project-wide index live in `CLAUDE.md`; the rules that govern the documentation system live in `docs/documentation-system.md`.

## Setup

- **Toolchain:** mise-managed (`mise install`): Bazel, Go, Node (commitlint only), lefthook — `.mise.toml` is the single home for every tool version, including Bazel's.
- `npm install` once for the commitlint dev dependency, then `lefthook install` to wire the git hooks.
- **macOS hosts (ix):** Command Line Tools suffice for the daemon (cgo + Virtualization.framework, verified); full Xcode is required only when building `RunnyBar` (rules_apple needs the SDK; Xcode is never opened — ADR-0007).

## Commands

| Command | Description |
| --- | --- |
| `bazel build //...` | Build everything buildable on this host |
| `bazel test //...` | Run the test suite |
| `bazel run //tools/format` | Format the tree (gofumpt, buildifier; swift-format once Swift lands) |
| `bazel run //:gazelle` | Regenerate BUILD files after import changes |

**Dependency workflow:** `go mod tidy -e` → `bazel run //:gazelle` → `bazel mod tidy`. Add Go deps at latest stable; Renovate keeps pins current.

**Known wrinkle (the `-e` and the ordering are load-bearing):** imports of `proto/runny/v1` resolve only in-graph (ADR-0006), so plain `go mod tidy` *aborts mid-attribution* and leaves every dep marked `// indirect` — which makes `bazel mod tidy` strip them from `use_repo` and breaks the build. `-e` tolerates the unresolvable import and attributes correctly. gazelle must run before `bazel mod tidy` so new BUILD references exist when use_repo is recomputed. gopls likewise can't resolve the generated import.

**Renovate and the bzlmod lockfile:** hosted Renovate bumps `MODULE.bazel` (and the bazel version in `.mise.toml`) but cannot run bazel, so `MODULE.bazel.lock` arrives stale and CI's `--lockfile_mode=error` refuses the PR. That refusal is the guard working. The `renovate-lockfile` workflow regenerates and pushes the lockfile to Renovate's PR branches automatically (bazel-version bumps included — new versions resolve different implicit deps). If it misfires, the manual flow still works: check out the branch, `bazel mod deps --lockfile_mode=update`, build/test, commit the lockfile, merge once CI re-proves it. Go-dep PRs never need this — gazelle's `go_deps` extension is reproducible and deliberately unrecorded in the lockfile.

## The Linux ↔ ix dev loop

Primary development happens on the Linux box; everything pure-Go builds and tests there. Darwin-only targets (`internal/vm`'s vz code, `cmd/runnyd`'s final binary, `RunnyBar`) need a macOS arm64 host:

```
rsync -a --exclude bazel-\* --exclude node_modules . brajkovic@ix:~/src/runny/
ssh brajkovic@ix 'cd ~/src/runny && bazel test //...'
```

The daemon binary must be codesigned with the `com.apple.security.virtualization` entitlement to boot VMs (ad-hoc signing is fine locally; see ADR-0008):

```
codesign -s - --entitlements tools/sign/runnyd.entitlements --force bazel-bin/cmd/runnyd/runnyd_/runnyd
```

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

GitHub Actions gate every PR and push to `main`: `ci.yml` (Linux: build/test/format; macOS: darwin targets + an ad-hoc-signed `runnyd` artifact; zizmor: workflow audit) and `pr-title.yml`. All checks must pass before merge. CI never boots guests — GitHub's macOS runners are VMs themselves, so VM-touching verification happens on a real host.

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

## Boundaries

- `bazel-*` symlinks and `node_modules/` are generated; never edit them.
- `MODULE.bazel.lock` is generated by Bazel; do not hand-edit.
- `.env` / `user.bazelrc` hold local settings or secrets; never commit them.
