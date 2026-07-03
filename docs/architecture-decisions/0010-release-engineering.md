# ADR-0010: Release engineering — release-please, stamped Bazel builds, Homebrew tap

**Status:** Accepted (2026-06-09)

## Context

runny needs versioned releases with changelogs, pre-release builds from
branches, supply-chain attestations, and a Homebrew installation path. The
obvious off-the-shelf composition is release-please (versioning/changelog
from conventional commits) + goreleaser (build, package, publish, brew tap).

## Decision

- **release-please** (manifest mode, `bump-minor-pre-major`) owns versions,
  CHANGELOG.md, tags, and GitHub releases. Starting manifest 0.0.0 so the
  first release PR computes 0.1.0 from the existing feat commits.
- **Versions are stamped by Bazel**, not baked into files: `tools/version.sh`
  derives the version from git — exactly the tag when on a tag; otherwise
  the conventional-commit-implied next version (computed by
  [svu](https://github.com/caarlos0/svu) with `--v0`, whose semantics match
  release-please's `bump-minor-pre-major`) with a `-beta.<shortsha>`
  pre-release suffix. The stamp flows through `--config=release` (`--stamp`
  + workspace status) into `main.version` via rules_go `x_defs`.
- **Branch/PR artifact builds** carry the pre-release stamp, so every build
  anywhere is identifiable; main-tag builds carry the release version.
- **goreleaser is rejected**: it drives `go build`, which cannot compile
  `cmd/runnyctl` (the runny.v1 protos exist only in-graph, ADR-0006), and
  the free tier cannot package prebuilt binaries. Two build systems would
  also violate ADR-0005's one-hermetic-graph decision. Release packaging is
  a short script: Bazel cold build → ad-hoc sign → tar + checksums → upload
  → attest.
- **Provenance attestations** (`actions/attest-build-provenance`) on every
  artifact-producing job, release and branch alike.
- **Homebrew tap** (`bojanrajkovic/homebrew-tap`): the release workflow
  renders the formula from `tools/deploy/runny.rb.tmpl` (version + url + sha256)
  and pushes it — the same work goreleaser's brew publisher would run. The tap
  also ships a cask (`runny-app`) for the GUI app, rendered the same way from
  `tools/deploy/runny-app.rb.tmpl`; the mutual-exclusion constraint lives
  formula-side (`conflicts_with cask: "runny-app"`). It authenticates as the
  **release bot App** (a GitHub App installation token scoped to `homebrew-tap`,
  `RELEASER_APP_ID` var, `RELEASER_APP_PRIVATE_KEY` secret), kept distinct from
  the runtime runner-registration App so CI and prod-host credentials don't share
  a blast radius. Skipped gracefully when those secrets are absent.
- **Beta channel via a second formula/cask pair**: a manually-tagged
  pre-release (see "Consequences" below) publishes as a GitHub pre-release,
  which the `homebrew` job detects via `github.event.release.prerelease` and
  renders into `runny-beta.rb`/`runny-app-beta.rb` (from
  `tools/deploy/runny-beta.rb.tmpl`/`runny-app-beta.rb.tmpl`) instead of the
  stable pair — so a beta publish never overwrites what `brew install runny`
  resolves to. The four packages (`runny`, `runny-beta`, `runny-app`,
  `runny-app-beta`) are mutually exclusive with each other: same binaries
  (`runnyd`/`runnyctl`) and, for the casks, the same `Runny.app` bundle
  identity (fixed by the Xcode product name, not something a cask
  controls) — one channel, one manager, at a time, same rule the stable
  formula/cask pair already followed. The beta formula declares the
  formula-formula and formula-cask conflicts (`conflicts_with`), same as
  the stable formula; neither cask declares `conflicts_with` (the
  `formula:` variant is deprecated with no replacement, per the existing
  comment in `runny-app.rb.tmpl`) — cask-vs-formula relies on each cask's
  `preflight` check, and cask-vs-cask relies on brew's own refusal to
  overwrite an app bundle it doesn't already own.
- Artifact-producing jobs stay **cold** (no Bazel or tool caches) per the CI
  security posture.

## Rejected alternatives

- **goreleaser**: see above; revisit only if the in-graph proto decision
  changes or prebuilt packaging lands in the free tier.
- **Version files committed by release-please** (`version.txt`, embedded
  constants): a second home for a fact git tags already own; stamping reads
  the tag.

## Consequences

- Manual pre-release tags are legal but must be named exactly what
  `tools/version.sh` stamps at that commit (`<next>-beta.<shortsha>`): svu
  and release-please share bump semantics but not baselines (latest tag vs
  the manifest), and a hand-named tag is how they diverge. Stable tags come
  only from release-please. The release workflow refuses a tag that
  disagrees with the version it stamps.
- RunnyBar distribution (later) follows the Tailscale shape: the .app
  bundles the daemon+CLI, signed and notarized — Developer ID signing
  upgrades this pipeline when certificates exist (see CONTRIBUTING.md).
  Note: the app (now Runny) ships standalone first (ADR-0016); the bundled
  shape, its lifecycle owner, and version coherence are decided in
  [ADR-0018](0018-bundled-app-distribution.md).
- release-please PRs are created with `GITHUB_TOKEN`, whose events do not
  trigger CI; the release PR shows checks only after a manual nudge or a
  PAT upgrade. Acceptable for now.
