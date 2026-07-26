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
  release-please's `bump-minor-pre-major`) with a `-beta.<count>.<shortsha>`
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
  `tools/version.sh` stamps at that commit (`<next>-beta.<count>.<shortsha>`): svu
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

## Amended: 2026-07-25 — Windows release artifacts

Windows hosts are a runner target (ADR-0026/ADR-0027), and `runnyd`/`runnyctl`
were being built ad hoc for that deployment rather than pinned to a release.
The `artifacts` job now also cross-compiles both `windows/amd64` and
`windows/arm64` (via `--platforms=@rules_go//go/toolchain:windows_{amd64,arm64}`,
already proven by `ci.yml`) and attaches
`runny_<version>_windows_{amd64,arm64}.zip` the same way as the darwin
tarball — checksummed and attested, no signing (`codesign_binary`/
`notarize_binary` are `target_compatible_with` macOS only, so these ship the
bare `stamped_go_binary` outputs).

**winget** manifests are submitted to the community `microsoft/winget-pkgs`
repo via `vedantmgoyal9/winget-releaser`, gated on the
`WINGET_PACKAGE_IDENTIFIER` repo variable and `WINGET_TOKEN` secret (same
graceful-no-op-until-configured shape as the Homebrew tap), and skipped on
pre-releases (winget has no beta-channel concept, so a beta tag must never
overwrite the stable manifest). `WINGET_TOKEN` — a classic PAT scoped
`public_repo`, able to open a PR against any public repo the account can
fork — lives in its own `winget` environment restricted to `v*` tags, the
same restriction the `release` environment already applies to the
signing/notary secrets; it does not share `releaser-app`'s environment,
since that credential's blast radius (an installation token scoped to
`homebrew-tap` alone) is deliberately narrower. **Rejected:** a self-hosted
winget source,
which needs a REST index service (no static-file equivalent of a brew tap
exists for winget) — disproportionate infrastructure for the reach gained.
This path has a one-time manual prerequisite the workflow cannot do for you:
fork `microsoft/winget-pkgs` under the same account, mint a classic PAT with
`public_repo` scope as `WINGET_TOKEN`, and submit the *first* manifest
version by hand (via `wingetcreate new`) — `winget-releaser` can only update
a manifest that already exists upstream.

**Chocolatey** ships as a release-asset-only `.nupkg`
(`runny.<version>.nupkg`, built with `dotnet pack` directly against
`tools/deploy/runny.nuspec.tmpl` — no placeholder `.csproj` needed as of
.NET 10) — install via `choco install <downloaded-path>`. **Rejected for
now:** a self-hosted feed via Sleet (a serverless static NuGet v3 feed
generator that can target S3/GitHub Pages, no server component) — mechanically
sound but needs a Sleet/dotnet publish step in CI and an unvalidated
GitHub-Pages-as-NuGet-v3-host spike; revisit once that's been proven out.
*(Superseded: the spike was run and the feed now ships — see the 2026-07-26
amendment below.)*
**Rejected:** pushing to the moderated `chocolatey.org` community repository
— an API key plus a review queue (days on first submission) for a channel
Homebrew's self-hosted-tap precedent argues against defaulting to.

## Amended: 2026-07-26 — the Chocolatey feed ships

The Chocolatey decision above deferred a self-hosted feed pending a spike:
whether GitHub Pages can serve a static NuGet v3 feed that Chocolatey will
actually consume. That spike was run end to end against a real release
artifact, and it passes, so the feed ships.

**A static v3 feed at [choco-feed](https://github.com/bojanrajkovic/choco-feed),
served from GitHub Pages**, generated by [Sleet](https://github.com/emgarten/Sleet)
and published by the `chocolatey-feed` release job. It is the Chocolatey
counterpart to `homebrew-tap` and mirrors that job's shape, including the
App-installation-token credential scoped to the feed repo alone and the
graceful no-op when the App is unconfigured. Users get the same experience
Homebrew users have:

```
choco source add -n=runny -s=https://bojanrajkovic.github.io/choco-feed/index.json
choco install runny
```

Three constraints the spike established, each of which shapes the setup:

- **Chocolatey 2.4.1 is the floor.** Chocolatey has read v3 feeds since 2.0.0,
  but on v3-*only* feeds — which is all Sleet produces — resolving an explicit
  `--version` was broken until [choco#3396](https://github.com/chocolatey/choco/issues/3396)
  landed in 2.4.1. This is the direct analogue of requiring a recent `brew`.
- **The feed's `baseURI` is absolute and baked into every generated file**, so
  the Pages URL is fixed at feed-init time; changing it means regenerating the
  feed rather than editing a config. Sleet's `path`, by contrast, is relative,
  so the committed config works from any CI checkout.
- **No beta/stable channel split is needed**, unlike the tap's separate
  `runny-beta` formula. One feed holds every version and Chocolatey only
  surfaces pre-releases when asked (`--pre`), so a beta tag publishes without
  clobbering what `choco install runny` resolves to.

**WinGet is dropped entirely, and its release job is removed.** The 2026-07-25
amendment's `winget` job never ran: it skips pre-releases, and every release
since it landed has been one. Its first real run would have *failed* rather
than published, because `winget-releaser` can only update a manifest that
already exists upstream and the one-time manual `wingetcreate new` submission
was never made. Removing the job also retires the `WINGET_TOKEN` PAT — the
broadest-blast-radius credential in this pipeline, able to open a PR against
any public repo the account can fork — along with its `winget` environment.
WinGet installs
portable packages into a non-version-qualified directory and refuses to replace
a running executable; it offers no author hook analogous to Chocolatey's
`chocolateyBeforeModify.ps1`, so a package cannot stop a service before its own
binary is swapped. Worse in practice: it installs machine-service binaries
under `%LOCALAPPDATA%` and then refuses to let an administrator uninstall them
("the package installed for user scope cannot be uninstalled when running with
administrator privileges"). A per-user path backing a `NT SERVICE\runnyd`
service is the wrong shape regardless of the tooling around it. Supporting the
daemon on WinGet properly would mean shipping an MSI, whose `ServiceControl`
table can stop and start a service — a larger change than this ADR takes on.

Chocolatey does not have that problem: `runnyctl install-daemon` resolves
`runnyd.exe` as a sibling of the running `runnyctl`, and because a Chocolatey
shim spawns the real binary, the service is registered against
`lib\runny\tools\runnyd.exe` — the path Chocolatey replaces in place on
upgrade — rather than against a shim or a user-profile symlink.

## Amended: 2026-07-26 — Windows upgrades are the package's job, not the daemon's

Shipping the Chocolatey feed made an existing gap load-bearing:
`runnyctl install-daemon` registers the service against
`lib\runny\tools\runnyd.exe`, which is inside the directory Chocolatey replaces
on upgrade. Windows locks the image file of a running process, so `choco upgrade
runny` against a live daemon would fail partway through the swap and could leave
the package directory half migrated.

**The package now owns the service lifecycle across an upgrade.**
`chocolateyBeforeModify.ps1` — the one hook that runs *before* Chocolatey
touches an installed package's files, on upgrade and uninstall alike — drains
and stops `runnyd`, escalating to a hard stop if the drain overruns, because
Chocolatey treats hook failure as non-blocking and would otherwise proceed into
the swap regardless. `chocolateyInstall.ps1` restarts the service only if that
hook stopped it. Neither registers a service: that stays `runnyctl
install-daemon`'s privileged, explicit act (ADR-0023), not a side effect of
unpacking a package. Chocolatey runs the hook from the version being upgraded
*from*, so it governs upgrades **from** the first release that ships it.

Before restarting, `chocolateyInstall.ps1` runs the **new** binary's
`-test-config` against the in-place config. That is the same compat question
darwin's `upgrade-daemon` exit gate asks of the respawn target, put at the only
moment Windows allows it: the new binary does not exist until Chocolatey has
written it, so it cannot be asked before the swap. It is advisory rather than a
gate — the service starts regardless, since withholding it would leave the fleet
down pending a second manual step and would let a false negative outweigh the
crash-loop it was avoiding, which runnyd already documents and announces itself.

**`runnyctl upgrade-daemon` now refuses off darwin instead of hanging.** Its
parse-deferral asks whether the binary the supervisor *would respawn* accepts a
config the running binary rejects — a question that only means something where a
newer binary can exist on disk while the old process runs. Homebrew produces
that state with a versioned cellar and a stable opt symlink; no Windows package
manager does. `systemRespawnTargetPath` states this per platform and is empty
off darwin, which both disarms the deferral and gates the refusal.

Before this, the darwin plist path was returned for any system daemon
regardless of platform, so an `upgrade-daemon` against a Windows system daemon
drained the whole fleet and then held the exit forever — the exit gate
re-consulting a file that could never exist, reporting "config.yaml not accepted
by the respawn target" about a config the running binary parsed perfectly.
Recovery needed an out-of-band SCM stop/start. Refusing before the drain, and
leaving nothing for the exit gate to consult, closes both halves.

## Amended: 2026-07-26 — pre-release versions have to sort

The pre-release suffix was `-beta.<shortsha>`, and that does not order. SemVer
compares prerelease identifiers containing letters lexically in ASCII order, so
`1.2.0-beta.8380a10f` outranks `1.2.0-beta.69d743dd` because `'8'` beats `'6'`.
A hex short SHA is effectively random, so roughly half of all betas sorted
*below* the beta they followed.

Homebrew never noticed: the tap job overwrites `runny-beta.rb` with whatever the
release rendered, so the newest publish wins by construction. A NuGet feed does
not work that way — it holds every version and resolves the highest, so
`choco upgrade runny --pre` correctly refused to move onto a build it considered
older than the installed one.

The suffix is now `-beta.<count>.<shortsha>`, where count is commits since the
last stable tag. A leading all-digit identifier compares numerically, so the
line orders monotonically; the SHA stays for traceability, since this string is
what `runnyctl version` reports. The count resets per release line, which is
harmless because the core version differs across lines and dominates comparison.
It is not zero-padded: SemVer already compares digits numerically and forbids
leading zeroes.

The counter has to be a pure function of the repository at that commit, because
`version.sh` runs twice — locally to compute the tag, and again in CI at that
tag, where the workflow asserts the two agree. That rules out CI run numbers and
build timestamps, which the local run could not reproduce.

Betas published under the old scheme sort *above* the new ones, since SemVer
ranks all-digit identifiers below alphanumeric ones. Both were removed from the
Chocolatey feed rather than left to shadow every future beta.
