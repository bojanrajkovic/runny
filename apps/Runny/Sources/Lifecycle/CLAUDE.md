# apps/Runny/Sources/Lifecycle

The app's daemon-lifecycle surface: registering `runnyd` as a per-user
LaunchAgent via `SMAppService`, the install/start/uninstall state machine, and
the post-upgrade respawn. Canonical docs: `docs/architecture/runny-app.md` and
ADR-0018. Sharp edges below.

## Shape

Same split as `Client/DaemonStore.swift`: a **pure decision layer**
(`LaunchAgentStatus.swift` — `nonisolated static` verdicts over plain value
types) plus a **thin side-effect wrapper** (`AgentController`) that owns the
`SMAppService`/`launchctl` calls behind a `ServiceRegistrar` protocol seam. The
verdicts are unit-tested without launchd; the side-effecting seam is mocked,
never exercised live in tests.

## Sharp edges

- **`SMAppService` success means *requested*, not *done*.** A `register()` /
  `unregister()` that returns without throwing only means launchd accepted the
  request. Confirmation is two-staged: the published state derives from
  `service.status` (never the call's return), and "is the daemon actually up" is
  a later `DaemonStore` `.connected` snapshot within a bound — never a checkmark
  off the throwing call.
- **`State` is closed and loud.** Every `SMAppService.Status` maps to a named
  case and a register/unregister THROW becomes `registrationFailed(reason:)` —
  there is no silent fall-through to `notInstalled`. `requiresApproval` (Login
  Items pending) is a first-class CTA, not a disguised failure; an unmodeled
  future status maps to `registrationFailed`, not a guess.
- **`.notFound` is two states in one hat, split by the bundled plist.**
  SMAppService returns `.notFound` both for a genuinely absent agent (a `bazel
  run` dev build carrying no injected plist) AND for a release whose bundled
  agent has *never been registered* — the pristine first launch, where the
  framework has no record of the (identity, plist) pair yet (distinct from the
  post-register/unregister `.notRegistered`). `state(from:bundledAgentPresent:)`
  resolves it by whether `Contents/Library/LaunchAgents/<plistName>` exists in
  the bundle (`AgentController.bundledAgentPresent`): present ⇒ `.notInstalled`
  (installable, toggle live), absent ⇒ `.notFound` (the honest "no bundled
  daemon"). Without the split, a first-time user's very first launch is stuck
  behind a disabled toggle and a false "no daemon" message — invisible to devs,
  whose machines read `.notRegistered` forever after a single register cycle.
- **The install location is canonical, not the running bundle.**
  `canonicalBundlePath` is `/Applications/Runny.app`; the agent's program path
  and the reconcile compare against it, **never** `Bundle.main.bundlePath` —
  that is the transient translocation mount on a `~/Downloads` launch, which
  would flag a perfectly-good `/Applications` agent as foreign.
- **Translocation refuses recoverably, never permanently.** Gatekeeper can
  transiently translocate even a correctly-installed `/Applications` app on its
  first launch, so the translocated verdict is "re-launch and retry", distinct
  from a non-translocated wrong location ("move to /Applications"). Translocation
  detection reuses the one safety heuristic in `Install/CLIInstaller.swift`
  (`isTranslocated` — the `…/AppTranslocation/…` / `/private/var/folders/` path
  match); both surfaces share that single definition rather than drifting copies.
- **Any `launchctl`/introspection carries a timeout.** There is no
  `bounded.Context` in Swift; a `launchctl print` can hang. A hung introspection
  surfaces "couldn't determine agent state" loudly, never spins. The shared
  `runLaunchctl` scaffold drains both pipes *before* `waitUntilExit`, so a verbose
  `print` can't deadlock on a full pipe buffer.
- **One spawn chokepoint.** Install, start-at-login enable, and Start all funnel
  through `attemptSpawn`, which consults the injectable `spawnGate` (default
  `.allow`; the Homebrew-reconcile step fills it) and aborts loud on deny. It
  returns its verdict rather than mapping errors itself — a `register` throw is a
  failed *install*, a `kickstart` throw a failed *start* with the agent still
  installed, so the two never bleed into one shared state. Never call
  `SMAppService`/`launchctl` from a view action; that bypasses the gate.
- **The Start recovery bound is healthy-magnitude × margin, not a budget sum.**
  `startRecoveryBound` is sized to a normal cold start, and recovery is confirmed
  from a later `.connected` snapshot — never the `kickstart` return. On expiry it
  is loud (`didNotComeUp`), never a silent spinner.
