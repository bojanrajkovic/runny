# ADR-0016: The Runny app — name, scope, and client toolchain

**Status:** Accepted (2026-06-12)

## Context

ADR-0001 reserved the Swift surface for a menu-bar app, and ADR-0006 made it
a symmetric sibling client of `runny.v1` over the unix socket. Building it
for real forces the decisions the placeholder name deferred: what the app is
(a glanceable status dot, or a real operator surface), how a Swift client
consumes the in-graph protobuf contract (ADR-0006 bans committed generated
code), what macOS it targets, whether it sandboxes, and how it ships before
the ADR-0010 bundled-distribution shape exists.

## Decision

- **Name and scope: the app is `Runny` (formerly RunnyBar), at `apps/Runny/`,
  with two surfaces.** A `MenuBarExtra` popover (visual `runnyctl status`)
  and a full main window (sidebar of runners; per-slot info, cycle timeline,
  logs). A menu-bar-only app caps out at "the dot is amber"; the questions
  that follow — *why*, *since when*, *what did the last cycle do* — need a
  real window. `LSUIElement` stays true; the app flips activation policy
  `.accessory` ↔ `.regular` while the main window is open, so it has no Dock
  presence until a window needs one. The rename is docs-only — `apps/` did
  not exist before this decision. ADR-0007 is amended in the same change:
  the window/activation-policy scope, and `Info.plist` as a checked-in file
  rather than generated from BUILD.
- **gRPC client: grpc-swift v1, vendored in-graph by rules_swift.**
  rules_swift 3.6.1's `swift_proto_library` client compilers generate v1
  stubs (the `GRPC` module, grpc-swift 1.16.0 as vendored) and bring the
  runtime and its SwiftNIO stack
  in-graph with BUILD overlays — zero new `MODULE.bazel` deps, no committed
  generated code (ADR-0006 holds). Verified: `//proto/runny/v1` analyzes
  with a `swift_proto_library` target. v1's `ClientConnection` dials unix
  sockets with plaintext h2c, which is the entire transport need.
  grpc-swift v2 (`GRPCCore`) has no in-graph path today — no BCR modules, no
  rules_swift overlays — so adopting it would mean a custom
  `swift_proto_compiler` plus a hand-built runtime dependency stack.
  **Migration to v2 is a recorded future fork**, keyed to rules_swift moving
  its vendored compilers, not to anything in this repo.
- **Live status is a supervised `WatchStatus` stream, not polling.** One
  server-push stream feeds the whole app, supervised by a connection state
  machine with explicit bounds: stream establishment is end-to-end bounded
  (dial *and* first snapshot, or teardown — a UDS `connect()` succeeds into
  the kernel backlog even when nothing is accepting, so connect alone proves
  nothing), and an open-but-silent stream is declared stale after missed
  server ticks. The living shape and the concrete bounds are in
  [`docs/architecture/runny-app.md`](../architecture/runny-app.md).
- **Command semantics: RPC success means *requested*, not *done*.** The app
  confirms Pause/Resume/Recycle from a subsequent `WatchStatus` snapshot and
  surfaces "not confirmed" when confirmation doesn't arrive — never a
  success checkmark off an RPC return alone.
- **Minimum macOS: 15.0.** Nothing in the app needs more than the
  `MenuBarExtra` floor, but the fleet the app is tested against runs macOS
  15+; claiming an older floor would advertise a configuration that never
  runs anywhere. The floor is the tested floor.
- **Not App-Sandboxed.** The app's one job is reaching the daemon's 0600
  socket under `~/.runny` (or a user-configured home), which lives outside
  any sandbox container. Hardened runtime and secure timestamp apply at the
  Developer ID tier, env-driven `CODESIGN_IDENTITY` like the binaries.
- **Distribution: a .dmg (drag-to-/Applications), built in-graph, with both
  the .app and the .dmg notarized and stapled** at the Developer ID tier
  (no-op pass-throughs at the ad-hoc tier, like the existing signing
  macros). Stapling everything means Gatekeeper assessment works offline on
  first launch.
- **The app ships standalone first.** ADR-0010's eventual shape — the .app
  bundles the daemon and CLI, Tailscale-style — remains the destination;
  it depends on an SMAppService install story that doesn't exist yet, and a
  standalone observer app is useful on day one to anyone already running
  runnyd. ADR-0010 carries a note to the same effect. That destination — the
  bundled `.app`, its `SMAppService` install, and Homebrew reconciliation — is
  decided in [ADR-0018](0018-bundled-app-distribution.md).

## Rejected alternatives

- **Keep the RunnyBar name**: accurate only for the popover. An app whose
  main surface is a full operator window named "Bar" misleads about scope,
  and the rename costs nothing before any code exists.
- **grpc-swift v2 now**: the actively developed line, and where this app
  should eventually land — but with no BCR modules and no rules_swift
  overlays it cannot be consumed in-graph today. Hand-rolling a
  `swift_proto_compiler` and its runtime stack to get there would be a
  build-engineering project larger than the app, purchased to avoid a
  migration that stays mechanical (stub surface and call sites) either way.
- **Polling `GetStatus`**: simpler client, no stream supervision — but it
  trades push latency for an interval, burns cycles when nothing changes,
  and *hides* daemon trouble: a poll loop happily renders the last good
  response while the daemon wedges. The supervised stream makes
  connected/reconnecting/unreachable/stale an explicit, rendered state —
  silent-failure-proofness is the project's optimization target.
- **App Sandbox + exceptions**: the sandbox's value is containing an app
  that handles untrusted input; this app talks to one local daemon the same
  user runs. Reaching an absolute socket path outside the container needs a
  temporary-exception entitlement — distribution-fragile outside the App
  Store, which this app will never enter (it exists to control a daemon the
  sandbox could never ship). A sandbox held open by exceptions is posture
  theater.
- **zip-only distribution**: cheaper to build, but a bare .app in a zip
  invites running from `~/Downloads` (translocation weirdness, no canonical
  install location) and there's no notarization-stapling story for the
  archive itself. The dmg pipeline exists in-graph anyway for the eventual
  ADR-0010 bundle; paying for it now is paying once.
