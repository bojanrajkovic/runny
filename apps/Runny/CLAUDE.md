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
  `NSApp.activate(ignoringOtherApps:)`; observe `NSWindow.willCloseNotification`
  and revert to `.accessory` only when no other regular, key-able, non-panel
  window remains. The main window is currently the only such window, but keep
  the check general — a second window that outlives the main one (e.g. a future
  Settings scene) must not strand the app `.regular` with nothing visible, and
  gating on the main-window identifier alone reintroduces that bug.
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
- **The home is fixed at `~/.runny`, no override.** `RunnyHome.directory` is
  unconditionally `homeDirectoryForCurrentUser/.runny` — there is no
  UserDefaults override, no `RUNNY_HOME`, no Settings field. This is what kills
  the app↔daemon split-brain (a Finder-launched app never sees shell exports,
  so a mismatched override silently rendered a healthy daemon "unreachable").
  The daemon derives the same path from its run-user's `$HOME`, so the two can't
  disagree. `diagnose()`'s "different home?" hint stays — it's still useful for
  the upgrade window where an *old* daemon was launched with `RUNNY_HOME`.
- **The daemon-card Reconnect is disabled while `reloadPending`, never during
  validation.** `reloadPending` is `pendingReload != nil` (the drain window with
  a live convergence verdict to lose); the guard exists so a manual re-dial
  can't tear down the stream and discard that verdict. It deliberately does *not*
  cover the earlier RPC-in-flight window — that's already made safe by
  `reloadTask` cancellation + the `reloadGeneration` bump, so don't broaden the
  guard to `reloadInFlight`. The disabled-while-pending direction is the view's
  `.disabled(store.reloadPending)` binding, verified in review (arming a real
  `pendingReload` needs the live RPC flow — no pure seam, by design); the unit
  test only pins that a fresh store leaves Reconnect enabled.
