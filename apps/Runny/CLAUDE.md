# apps/Runny

The SwiftUI app: menu-bar popover + main window over the `runny.v1` socket.
Canonical docs: `docs/architecture/runny-app.md` (shape, connection FSM,
bounds) and ADR-0016 (decisions). Sharp edges below.

## Sharp edges

- **Cycle order is NOT proto enum order.** `SLOT_STATE_SECURE_SSH` was
  appended for wire compatibility and sits mid-cycle; numeric order already
  diverges from cycle order — render by the explicit mapping in
  `Sources/Client/SlotStateDisplay.swift`, never by raw enum value. The
  comment on the enum in `proto/runny/v1` is the contract-side warning.
- **Window/activation mechanics are a pattern, not a one-liner.** Open the
  main window via `openWindow(id:)` only after
  `NSApp.setActivationPolicy(.regular)` plus a main-queue-hopped
  `NSApp.activate(ignoringOtherApps:)`; revert to `.accessory` on
  `NSWindow.willCloseNotification` filtered by window identifier, and only
  when no other regular windows remain (Settings counts). Deviating gets a
  window that opens behind everything, or an app that vanishes from the Dock
  while Settings is still open.
- **`MenuBarExtra` `.window` style has no public programmatic dismissal.**
  Closing the popover (e.g. on "Open Runny") goes through a minimal AppKit
  shim that finds and closes the panel `NSWindow` — check whether the
  current macOS gained a real API before extending the shim.
- **grpc-swift v1 idioms, not v2.** Stubs come from rules_swift's vendored
  v1 compilers (`GRPC` module): `ClientConnection` over the UDS target,
  plaintext, with the generated `Async`-prefixed client wrappers. Do not
  import or imitate `GRPCCore` (v2) patterns — there is no in-graph path to
  that runtime (ADR-0016).
- **Building needs full Xcode** (SDK vendor only, never opened — ADR-0007).
  Command Line Tools alone fail at analysis; `bazel query` still works as a
  syntax check.
- **SwiftUI conventions:** tqbf's rules apply throughout —
  <https://gist.github.com/tqbf/0d228f1e79e00a39a6973066be3f62c3>. The ones
  that bite here: TimelineView at leaf Text only, no insert/remove
  transitions in hosted trees, static DateFormatters, explicit rebuilds of
  `@Observable` snapshot caches.
- **Version skew compares `x.y.z` cores.** The daemon publishes its full build
  label (`0.6.0-beta.<sha>`) while the app's `CFBundleShortVersionString` is
  already regex-stripped to its core, so `skewVerdict` normalizes the daemon
  string with `versionCore` (anchored at the start, mirroring the build's
  `re.match`) before comparing — a raw compare false-alarms on every beta/CI
  build (same commit, two spellings). The warning **names the cores, not the
  daemon's full sha-bearing string**, so a same-core rebuild that only rotates
  the build sha can't re-pop a dismissed banner; the full daemon version is
  shown in the `runnyd <version>` line above. The human version is
  `CFBundleShortVersionString`, not `CFBundleVersion`; a missing read coalesces
  to the `unstampedVersion` sentinel (the build's `fallback_build_label`), the
  same quiet branch a dev build takes, so a wrong key fails safe (quiet), never
  loud-and-wrong.
- **`expectedProtocolVersion` is kept in lockstep with the daemon's
  `WireProtocolVersion` — bump both together.** It is the exact protocol the
  app's stubs were built against, not a backstop or a cap; the protocol axis
  fires only when the daemon is *behind* (`<`), the upgrade-window case the
  matched `x.y.z` cores hide.
- **Skew warns, never refuses, and is a row/banner, never an `.alert`.** No
  control is disabled on `store.skew` (verify by the *absence* of any
  `.disabled(store.skew…)` / connection-drop in review, not a sham test); a
  standing condition rendered as a re-popping modal would be alarm fatigue.
  Read the verdict's `kind`, never string-match its `text`. Both surfaces read
  the connection-gated `visibleSkew` (the card) / `shownSkew` (the dismissible
  popover banner) — never `skew` directly, so a stale verdict can't outlive the
  live stream and lie about a daemon that may have recycled.
