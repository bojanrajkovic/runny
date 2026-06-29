# ADR-0018: Bundled distribution — the app installs and version-locks the daemon

**Status:** Accepted (2026-06-16)

**Amended (2026-06-19):** the networking premise below was overturned by a
spike. This ADR states that "a system daemon of any uid is silently denied"
local network access, and that only a future userspace network stack could
admit a headless daemon. That is backwards. Per Apple's TN3179 and a spike on a
pristine, never-logged-in host, **any daemon started by launchd is auto-allowed
local network access regardless of uid**; the *gated* path is the per-user
**LaunchAgent**, because macOS keeps local-network privacy state per user
account (hence its one-time prompt). The genuinely denied case is a process
that self-daemonizes / reparents away from launchd. The per-user-agent decision
recorded below still describes runny's current shipping shape and stands — but
its *rationale* does not: a headless, non-root **system LaunchDaemon** is viable
today with **no** change to the networking substrate (vmnet stays), and the
userspace network stack is demoted to optional hardening. That headless path is
tracked in [#76](https://github.com/bojanrajkovic/runny/issues/76), and the ADR
that supersedes the contingency framing here is
[ADR-0020](0020-headless-system-daemon.md); the userspace-stack hedge moved to
[#84](https://github.com/bojanrajkovic/runny/issues/84). The body below is
preserved as the decision-time record — read it through this banner.

**Superseded in part (2026-06-26, [ADR-0023](0023-app-non-privileged-boundary.md)):**
the **CLI-vending** decision below ("Vend the CLI by symlinking
`/usr/local/bin/runnyctl` into the bundle") is withdrawn — the app is now
non-privileged and no longer installs the CLI. `runnyctl` reaches PATH via the
Homebrew tap formula or by running it from inside the bundle. The bundling of
`runnyd`+`runnyctl`, the per-user `SMAppService` LaunchAgent, the version-lock,
loud skew, and the *observe-don't-manage* detect-and-defer against a system daemon
all stand; only the app's privileged CLI install goes.

**Amended (2026-06-24, [ADR-0022](0022-app-driven-runnyd-updates.md)):** the
Consequence below that a system daemon is offered "only the generic skew banner,
not a futile fleet-draining update" is superseded. The installed system daemon
now gets a real, config-gated update — an app-brokered atomic re-stage + reload,
or operator-driven `runnyctl upgrade-daemon` on a headless host — each gated on
the new binary validating the in-place config. The bundling, version-lock, and
loud-skew decisions here stand; only the no-system-daemon-update stance changes.

**Amended (2026-06-28):** the app now surfaces a **notify-only** update check
for the direct-`.dmg` channel. "The `.dmg` is installed independently of
`brew`, so an operator can upgrade one channel and not the other" (Consequences
below) now has an in-app signal: the app polls `api.github.com/.../releases/latest`
at launch and every 24 h, and shows a dismissible banner when a newer release is
available — linking to the release page and naming `brew upgrade --cask runny-app`
as the fast path. The app **never downloads or replaces itself**; brew/cask remains
the actual updater. Sparkle was weighed and rejected: ~4–6 engineering days plus a
third signing key, a hosted feed, and a per-release publish-and-validate step,
buying only true one-click self-update for the direct-`.dmg` holdout; that does not
clear the bar when the cask already provides auto-update. The full analysis is in
the [design doc](https://outline.gaur-kardashev.ts.net/doc/142-app-update-notify-brew-is-the-updater-no-sparkle-hE03z9db65).
Closes [#142](https://github.com/bojanrajkovic/runny/issues/142).

## Context

A release stamps all three artifacts — `Runny.app`, `runnyd`, `runnyctl` — at
one version in a single `release.yml` run, then ships them down **two** install
channels: a Homebrew tarball (`runnyd`+`runnyctl`, for headless fleet hosts) and
a `.dmg` (the app, installed standalone by hand). `runnyd` and `runnyctl`
co-ship in the tarball, so they cannot skew against each other. The skew axis is
**app ↔ binaries**: the `.dmg` is installed independently of `brew`, so an
operator can upgrade one channel and not the other.

That drift degrades **silently**. Every cross-version feature is gated on a
monotone `protocol_version` with graceful fallback ([ADR-0017](0017-confirming-reload-convergence.md)):
an older daemon makes a newer client quietly do less — a silent failure, the one
failure mode runny exists to eliminate. The reload-convergence review repeatedly
circled this new-client/old-daemon window.

[ADR-0010](0010-release-engineering.md) and [ADR-0016](0016-runny-app.md) both
named the eventual "Tailscale shape" — the `.app` bundles the daemon and CLI —
and both **deferred** it on a missing `SMAppService` install story. This ADR
decides that shape, and the ownership fork it turns on: who runs the bundled
daemon.

**Prior art settles the mechanics.** Tailscale, Docker Desktop, and OrbStack
each ship one signed app carrying a GUI, a background component, and a CLI, and
they converge on the same answers. The CLI is exposed by symlinking
`/usr/local/bin/<cli>` *into* the bundle (OrbStack's `orb`/`orbctl`/`docker` →
`OrbStack.app/Contents/MacOS/…`), which version-locks it to the app for free; a
separate CLI binary like `runnyctl` fits this directly. A bundle-embedded
launchd job names its program by a **bundle-relative `BundleProgram`** with an
`AssociatedBundleIdentifiers` back-reference — the `SMAppService` format, on disk
in Tailscale's `tssentineld` agent. And every one of these tools treats its app
and a Homebrew install as **mutually exclusive** (Tailscale's formula declares
`conflicts_with` its cask) rather than making two managers coexist. The heavier
mechanisms they also carry — a root privileged helper
(`dev.orbstack.OrbStack.privhelper`, `com.docker.vmnetd`) or a VPN System
Extension (Tailscale) — exist for capabilities runny does not need.

## Decision

The `.app` becomes the **GUI install channel for all three artifacts**, the
**installer (never the parent) of a launchd-managed daemon**, and the place
residual skew is **made loud**.

- **Bundle notarized `runnyd` + `runnyctl` in the `.app`.** One `.dmg`, one
  stamped version, one upgrade action — the persistent cross-channel skew
  cannot exist for an app user by construction.

- **Install the daemon as a per-user LaunchAgent via `SMAppService`; launchd
  owns the process, the app never does.** Crash-only `KeepAlive` is the daemon's
  recovery model ([ADR-0012](0012-wedged-guest-escalation.md),
  [ADR-0014](0014-config-reload-drain-and-respawn.md)): on a wedge or a reload it
  exits non-zero for a launchd cold start. An app that `Process`-spawned `runnyd`
  would kill it on quit and never restart it after a wedge — silently defeating
  the recovery the daemon is built around. The app registers, enables, updates,
  and `kickstart`s the agent; launchd runs it. The app registers an
  `SMAppService.agent` (per-user), not an `SMAppService.daemon` (system) — a
  choice **contingent on the current networking substrate**, not a standing
  posture. runny reaches guests over vmnet/NAT, which macOS gates behind a Local
  Network grant only a GUI-session process can hold ([`deploy.md`](../deploy.md)
  "Why this is not just `launchctl load`",
  [ADR-0008](0008-native-virtualization-framework.md)); a system daemon of any
  uid is silently denied. (This premise was overturned — see the amendment at
  the top of this ADR; a headless non-root system LaunchDaemon is shipped in
  [ADR-0020](0020-headless-system-daemon.md).)

- **Point the agent at the `runnyd` inside the app bundle — the native
  `SMAppService` shape.** The agent's plist ships at
  `Contents/Library/LaunchAgents/com.coderinserepeat.runnyd.plist` and names the
  daemon by a bundle-relative `BundleProgram`, so the job points into the current
  bundle by construction; an app upgrade replaces the bundle and a post-upgrade
  `kickstart` moves the running daemon onto the new binary — the version coupling
  is the point. This requires the app to live at a stable path (`/Applications`),
  which the `.dmg` drag enforces and `SMAppService` requires anyway: a
  translocated `.app` run from `~/Downloads` cannot register an agent pointing
  into itself (the reason ADR-0016 chose `.dmg` over zip).

- **Vend the CLI by symlinking `/usr/local/bin/runnyctl` into the bundle.** An
  "Install command-line tool" action (VS Code `code`-style, OrbStack's
  `orb`/`docker` precedent) links the bundled `runnyctl`, version-locking the CLI
  to the app for the same reason the agent is — so an app user never separately
  `brew`-installs the CLI.

- **Fix the home at `~/.runny` for every channel; remove `RUNNY_HOME`.** The home
  is non-configurable everywhere — the env-var override is deleted, not merely
  hidden in the app; the daemon always derives `~/.runny` from its own run-user
  (forward-compatible with #76's dedicated user). A switchable home is the entire
  source of #67's hazard cluster
  ([#67](https://github.com/bojanrajkovic/runny/issues/67)) and buys nothing a
  symlink cannot, so the fix deletes the switch: the configurable-home Settings
  surface and the `restart()`-on-home-change machinery go with it, and the `brew`
  and manual installs drop `RUNNY_HOME` from their service definitions.

  **Amended (2026-06-19, #76):** "the daemon always derives `~/.runny`" is now
  deployment-*resolved*, not fixed to one literal path. The home stays
  non-configurable — still no env/flag override, nothing for a user to flip — but
  a non-root system daemon resolves to a fixed `/Library/Application Support/runny`
  it owns, while a per-user agent keeps `~/.runny`. The daemon selects by
  ownership (it must own the tree it binds) and clients by existence (the operator
  reaches the system home through an inheriting ACL granting dir-write, not
  dir-ownership), so daemon and clients can never disagree about where the socket
  and credentials live. This keeps the no-switchable-home invariant intact while
  letting a headless service account run with no home directory of its own.

- **Render residual skew; never assume it gone.** Bundling cannot close two
  gaps: the **upgrade window** (launchd runs the *old* daemon binary until the
  next recycle, so a freshly launched new app talks to an old daemon — exactly
  the `protocol_version` case) and a **shared host** that also runs a
  brew-managed daemon. The app compares its stamped bundle version and expected
  protocol against the daemon's live `version`/`protocol_version` and surfaces a
  first-class skew state — a warning, never a refusal (Tailscale's CLI surfaces
  the same daemon-vs-client mismatch over its IPC socket); after an app upgrade it
  offers to `kickstart` the agent onto the new binary, gated on a drain since
  crash-only forbids interrupting a job. The verdict is a pure function beside the
  reload/ack verdicts in `DaemonStore`, unit-tested without a live daemon.

- **The system LaunchDaemon is the headless channel; reconcile by detect-and-defer.**
  The installed non-root system LaunchDaemon (`sudo runnyctl install-daemon`) is the
  path for fleet hosts with no GUI; the Homebrew formula delivers the binaries only.
  The app installs its per-user agent **only when no system daemon owns the home**;
  if it detects an installed system daemon (any verdict other than
  `unmanaged`/`selfManaged`/`awaitingApproval`) it does not install its own, acts as
  an observer, and points the operator at Settings → System Service. The app's
  per-user agent and the system daemon share the canonical label
  `com.coderinserepeat.runnyd` (they differ only by launchd domain — `gui/` vs
  `system/`); they are mutually exclusive owners of one home. Mutual exclusion is the
  industry norm (Tailscale's `conflicts_with`); runny enforces it at runtime rather
  than at package install because both channels are legitimate for different
  audiences.

## Rejected alternatives

- **App `Process`-spawns the bundled `runnyd`.** The obvious "bundle and run."
  It dies with the app and never restarts after a wedge — defeating crash-only
  `KeepAlive`, the daemon's whole recovery model. This constraint is *why* the
  app installs an agent rather than running the binary.

- **A root privileged helper or System Extension** (the Docker/OrbStack plumbing
  path; Tailscale's VPN System Extension). On the current vmnet model runny's
  daemon needs no root — only the per-user Local Network grant in a GUI session —
  so a per-user LaunchAgent suffices, and a root helper (`SMJobBless` / an
  `SMAppService` daemon) or a System Extension would only add a System-Settings
  approval step and a privileged attack surface for capabilities runny does not
  use. Dropping the GUI-session requirement is a separate question — a *non-root*
  headless LaunchDaemon behind a userspace network stack
  ([#76](https://github.com/bojanrajkovic/runny/issues/76)), not a root helper
  bolted onto vmnet.

- **The app as the sole channel (retire `brew`).** Headless fleet hosts have no
  GUI to run the app; `brew` is the only sane install there. Keep both, split by
  audience.

- **Bundle the binaries but leave lifecycle to the operator** (the app stays a
  pure observer + CLI vendor). Smaller forever, never touches `SMAppService`/TCC
  — but it leaves "the daemon isn't running / is the wrong version" to the human
  and never delivers the one-click story the bundled shape exists for. A
  legitimate lighter shape; rejected because the decided destination is the
  managed agent. (The app stays observer-*capable* — see Consequences — but that
  is its degraded mode against a system daemon, not its purpose.)

- **Treat bundling as sufficient to eliminate skew — no detector.** Bundling
  cannot close the upgrade window or the shared-host case; assuming it does
  reintroduces the silent failure this effort targets. The detector is
  mandatory, not optional polish.

- **A version handshake that refuses to connect on mismatch.** Docker and
  OrbStack hard-refuse a client below a minimum API version; runny does not. Its
  contract is a monotone `protocol_version` with graceful degradation
  (ADR-0017), and a hard refusal would break the legitimate new-client/old-daemon
  upgrade window that a crash-only recycle resolves on its own. Skew must be
  visible, never fatal — the warn-and-continue behavior Tailscale's CLI already
  models (`client version != tailscaled server version`, printed, non-fatal).

- **Point the agent at a `runnyd` copy outside the bundle** (e.g.
  `~/.runny/bin`). Decouples the daemon from the app's presence, but reintroduces
  a second on-disk copy that can skew from the bundle plus an install/uninstall
  dance to keep them in sync — re-creating the drift the bundle removes, and
  forgoing the bundle-relative `BundleProgram` that version-locks for free.

## Consequences

- Supersedes the deferred-bundling notes in ADR-0010 (release-engineering
  consequences) and ADR-0016 (the app "ships standalone first"); back-pointers
  land on all three in the same commit. The app stays standalone-*capable* — it
  observes a brew/manual daemon untouched — but the bundled, app-installed shape
  is now the decided destination, not a deferral.
- Closes [#67](https://github.com/bojanrajkovic/runny/issues/67): the fixed home
  removes the restart-race, stale-cache, absent-home-watcher, and
  settings-split-brain hazards by deleting the home-switch path, not patching it.
- [Issue #62](https://github.com/bojanrajkovic/runny/issues/62) is the
  implementation tracker for the staged rollout (loud-skew detector first, then
  bundling + CLI vending, then the `SMAppService` agent + reconciliation); the
  slices live there, not in this ADR.
- The app gains an installer/updater surface — start-at-login, install/repair the
  daemon agent, install the CLI, uninstall the daemon. Uninstalling must
  `bootout` the agent: an orphaned agent whose program lived in a deleted `.app`
  would loop launchd against a missing binary.
- The TCC Local Network grant is keyed to binary identity, so first run of the
  app-bundled `runnyd` re-prompts even on a host that previously granted a `brew`
  binary; the app must surface the prompt (the reason `runnyd` runs foreground
  today), never let it fail silently as "no route to host."
- The bundled `runnyd` must keep its `com.apple.security.virtualization`
  entitlement — signed and notarized in-graph already (ADR-0010), so the bundled
  copy carries it; the bundling must not strip it.
- runny carries no root helper and no System Extension; the per-user LaunchAgent
  plus TCC grant is the entire privileged surface — smaller than the container
  tools' root helpers.
- **The per-user agent is contingent, not axiomatic.** `SMAppService.agent` is
  correct only while guests ride vmnet and its GUI-session Local Network gate.
  A headless non-root system LaunchDaemon (`runnyctl install-daemon`) is shipped
  in [ADR-0020](0020-headless-system-daemon.md) for fleet hosts that have no GUI
  session. The bundling, version-locking, CLI-vending, and detect-and-defer
  decisions above survive unchanged for the app-managed path; the optional
  userspace-network-stack hardening ([#84](https://github.com/bojanrajkovic/runny/issues/84))
  remains a future hedge.
- CLI vending targets `/usr/local/bin`, which on an Apple-Silicon host may not
  exist or be writable without an admin prompt; the create-and-symlink step is an
  implementation detail tracked in #62.
- The CLI-symlink detect-and-defer is the counterpart of the daemon's (ADR-0019),
  refined in two ways the reference apps get wrong. A refused foreign owner names its
  *channel* (Homebrew, a hand-rolled link, a dropped file) with remediation, not just
  the path. And the drag-to-trash orphan — `/usr/local/bin/runnyctl` left dangling
  when the bundle is deleted, which macOS gives no hook to clean — is reconciled on a
  later launch by *surfacing* it (a distinct `orphaned` state offering Remove or
  re-point), never by silently re-writing the link on every launch. Docker Desktop
  re-links on every launch, which both re-raises the admin prompt
  ([for-mac#6634](https://github.com/docker/for-mac/issues/6634)) and clobbers a
  Homebrew-managed CLI ([for-mac#455](https://github.com/docker/for-mac/issues/455));
  Tailscale does no CLI reconcile at all. Surface-don't-rewrite and never-clobber
  avoid both, at the cost that an orphan persists until a Runny runs again — the
  unavoidable residue of having no uninstall hook.
- The release still emits both the `brew` tarball (headless) and the `.dmg` (now
  carrying the binaries) — the same *source* binaries, but the `.app` re-signs its
  nested copies inside-out, so their CDHash differs from the tarball's separately
  notarized ones. Each container is signed and notarized on its own; the
  version-skew detector, not a CDHash match, is what proves two installs agree.
- `docs/deploy.md` gains the app-managed install path beside `brew` and manual,
  and states the audience split; `docs/security.md` notes the app as a daemon
  installer under the existing signing posture.

## Prior art

- Apple, [`SMAppService`](https://developer.apple.com/documentation/servicemanagement/smappservice)
  — the bundle-embedded `Agent`/`Daemon`/`LoginItem` registration this builds on.
- Tailscale, [three ways to run Tailscale on macOS](https://tailscale.com/kb/1065/macos-variants)
  — the GUI/standalone/CLI split and its bundled-vs-Homebrew exclusivity.
- Tailscale's Homebrew formula declares `conflicts_with` the cask — precedent for
  one-manager-only; and the CLI surfaces a daemon-vs-client
  [version mismatch](https://github.com/tailscale/tailscale/issues/17667) over
  IPC — precedent for warn-not-refuse.
- OrbStack and Docker Desktop symlink their CLIs from `/usr/local/bin` into the
  app bundle (observed on disk) — the version-locked CLI-vending pattern.
