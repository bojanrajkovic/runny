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
