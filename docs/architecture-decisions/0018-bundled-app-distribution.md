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
tracked in [#76](https://github.com/bojanrajkovic/runny/issues/76), which will
carry the ADR that supersedes the contingency framing here; the userspace-stack
hedge moved to [#84](https://github.com/bojanrajkovic/runny/issues/84). The body
below is preserved as the decision-time record — read it through this banner.

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
  uid is silently denied. A future userspace network stack would lift that gate
  and admit a headless, non-root LaunchDaemon — the deliberate not-yet fork in
  [#76](https://github.com/bojanrajkovic/runny/issues/76) (see Consequences).
  Until then, per-user is the only configuration that reaches its guests at all.

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

- **Homebrew stays the headless channel; reconcile by detect-and-defer.** `brew`
  and the documented manual LaunchAgent remain the path for fleet hosts with no
  GUI. The app installs its agent **only when no other manager owns the daemon**;
  if it detects an externally-managed `runnyd` — a `brew` `homebrew.mxcl.runny`
  agent, or the manual `com.coderinserepeat.runnyd` — it does not install its
  own, acts as an observer, and points the operator at the managing channel. The
  app-managed and manual agents share the canonical label
  `com.coderinserepeat.runnyd`; they are mutually exclusive installers of one
  label. Mutual exclusion is the industry norm (Tailscale's `conflicts_with`);
  runny enforces it at runtime rather than at package install because both
  channels are legitimate for different audiences.

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
  is its degraded mode against a foreign daemon, not its purpose.)

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
  correct only while guests ride vmnet and its GUI-session Local Network gate. A
  future move to a userspace network stack (`VZFileHandleNetworkDeviceAttachment`
  + e.g. gvisor-tap-vsock) would lift the gate and admit a **headless,
  dedicated-non-root LaunchDaemon** (`SMAppService.daemon` + `UserName`), removing
  the auto-login requirement on fleet hosts — a substantial research fork that
  revisits ADR-0008 and is out of scope here
  ([#76](https://github.com/bojanrajkovic/runny/issues/76)). If it lands, the
  bundling, version-locking, CLI-vending, and detect-and-defer decisions above
  survive unchanged; only the registration call and the home-as-GUI-session
  assumption shift.
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
