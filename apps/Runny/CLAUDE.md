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
  window remains. The main window and the Settings scene **both** call
  `ActivationCoordinator.windowAppeared()` (deliberately not named for either),
  so whichever outlives the other keeps the app visible until both are gone —
  gating the revert on the main-window identifier alone reintroduces the
  strand-`.regular`-with-nothing-visible bug.
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
- **The home is deployment-resolved (system-then-per-user), no override.**
  `RunnyHome.directory` is `/Library/Application Support/runny` when that dir
  EXISTS, else `homeDirectoryForCurrentUser/.runny` — there is no UserDefaults
  override, no `RUNNY_HOME`, no Settings field. This is what kills the app↔daemon
  split-brain (a Finder-launched app never sees shell exports, so a mismatched
  override silently rendered a healthy daemon "unreachable"). Resolution mirrors
  Go's `home.ResolveClient` (existence selection), so the app and a system daemon
  resolve the same home and can't disagree. `diagnose()`'s "different home?" hint
  stays — still useful for the upgrade window where an *old* daemon was launched
  with `RUNNY_HOME`.
- **The WHOLE home resolves, and the socket + artifacts derive from it — one
  axis, so they can't diverge.** `RunnyHome.socketPath` is `directory/runnyd.sock`
  and `artifactURL` reads `cycles/` from `directory`; keying the socket on a
  separate socket-file-existence axis would let the app dial one home while
  reading artifacts from another. Selection is by dir existence; LIVENESS is the
  daemon connection's job — the `DaemonStore` gRPC `WatchStatus` stream (Go's
  stat-for-selection / connect-for-liveness split). The ownership install gate no
  longer has a socket axis: it reads only the `system/`-domain launchd probe + the
  app's own SMAppService self-status. `ensureDirectory` only ever creates the
  **per-user** home (its default target), never the installer-owned system home;
  dir-existence resolution makes this safe-by-construction (the system home only
  wins when it already exists). On a host with a system install, the app targets
  the system daemon even when it is *down* (reports "down" rather than silently
  dialing a live per-user socket) — the system-ahead-of-per-user precedence the
  ownership verdict already enforces. **`RunnyHome.systemHomeDir` MUST stay in
  sync with Go's `home.SystemHomeDir`** (`internal/home/home.go`): Swift can't
  import the Go const, so both hardcode the path, and `RunnyHomeResolutionTests`
  pins the literal as the drift guard.
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
- **The bundle carries `runnyd`/`runnyctl`, signed inside-out by `codesign_app`
  — never re-introduce an outer-only seal.** `codesign_app` places each binary at
  `Contents/MacOS/<bare-name>`, signs it nested-first with its own entitlements
  (the daemon gets `com.apple.security.virtualization`, the CLI gets none — both
  asserted at build time, the CLI's absence too), then seals the bundle over them.
  A single outer `codesign` of the `.app` does **not** cover nested Mach-Os, so an
  outer-only seal ships an unsigned binary that fails Gatekeeper on a clean
  machine. The verification `--deep` in the macro is a *different* operation from
  the deprecated signing `--deep` the project avoids; don't "consistency-fix" them
  together. `additional_contents` is deliberately **not** used for these — its
  `else`-branch would land `runnyd_signed.bin` at the wrong filename and unsigned.
- **The app does not install the CLI, and raises no admin prompt, ever.** The app
  is non-privileged ([privilege boundary](../../docs/architecture/privilege-boundary.md),
  ADR-0023): it neither symlinks `runnyctl` into `/usr/local/bin` nor manages a
  system daemon. `runnyctl` reaches PATH via the Homebrew cask or from inside the
  bundle; the bundle still *carries* it (signed inside-out, no entitlements). A
  registered `system/` daemon is **observed** read-only (the unprivileged ownership
  probe drives the observer banner pointing at `sudo runnyctl uninstall-daemon`), never
  installed/updated/removed by the app. There is no `with administrator privileges`
  line, `osascript` broker, or privileged subprocess anywhere in the app target —
  that absence is the boundary; verify it stays gone in review.
- **Daemon lifecycle lives in `Sources/Lifecycle/` — read its `CLAUDE.md` for the
  sharp edges, don't duplicate them here.** The load-bearing ones a broad change
  must respect: `SMAppService` success means *requested*, so `installState` is
  derived from `service.status`, never a call return; every spawn-triggering action
  funnels through `AgentController.attemptSpawn` (the gate now carries the
  daemon-ownership verdict — install/repair/start are denied for a `systemManaged`,
  `awaitingApproval`, or `indeterminate` owner) — no view calls `SMAppService`/`launchctl` directly; the bundled
  LaunchAgent plist is `BundleProgram`-relative and is NOT the host-install
  template (two shapes, do not unify); the Local Network grant card reads the
  **daemon-published `local_network_grant`**, never the button-gated `doctorChecks`;
  uninstall best-effort-`bootout`s ("No such process" = success), never kills; the
  reconcile compares the **canonical `/Applications/Runny.app`**, never
  `Bundle.main.bundlePath` (the translocation mount on a `~/Downloads` launch).
