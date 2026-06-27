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
- **The reconcile parses TWO launchctl program forms — our real agent is the
  second.** `launchctl print` reports a *hand-installed* plist's `Program` as an
  absolute `program = /path` (compared to canonical), but our own `SMAppService`
  agent registers a bundle-relative `BundleProgram`, so launchd prints
  `program identifier = Contents/MacOS/runnyd` (with `parent bundle identifier =
  com.coderinserepeat.runny`) and **no `program =` line at all**.
  `parseLaunchctlProgram` returns `.bundleProgram` (→ reconcile `.ok`) for that
  shape when the parent id is ours AND the program names the bundled daemon
  (`bundledAgentRelativeProgram`, comparing the path before launchctl's
  " (mode: N)" suffix) — canonical by construction, since only our registration
  produces *that* relative program under our label and the install gate already
  refused a non-canonical bundle. A registration under our id pointing at a
  different `BundleProgram` stays `.undetermined` (the gate probes
  `Contents/MacOS/runnyd`, so `.ok` must mean exactly that binary). Matching only `program =` (the bug a
  live in-place upgrade caught — the unit fixture had fabricated an absolute line
  no real `SMAppService` agent emits) leaves every real per-user agent
  `.undetermined`, which silently hides the post-upgrade Update affordance. The
  parser keys off `program = ` vs `program identifier = ` (the space-equals
  disambiguates), so the order of the two checks is load-bearing.
- **Translocation refuses recoverably, never permanently.** Gatekeeper can
  transiently translocate even a correctly-installed `/Applications` app on its
  first launch, so the translocated verdict is "re-launch and retry", distinct
  from a non-translocated wrong location ("move to /Applications"). Translocation
  detection lives in `Translocation.isTranslocated` (the `…/AppTranslocation/…` /
  `/private/var/folders/` path match) — a plain, non-privileged path heuristic.
- **Any `launchctl`/introspection carries a timeout.** There is no
  `bounded.Context` in Swift; a `launchctl print` can hang. A hung introspection
  surfaces "couldn't determine agent state" loudly, never spins. The shared
  `runLaunchctl` scaffold drains both pipes *before* `waitUntilExit`, so a verbose
  `print` can't deadlock on a full pipe buffer.
- **One spawn chokepoint, gated on ownership.** Install, repair, start-at-login
  enable, and Start all funnel through `attemptSpawn`, which consults the
  `spawnGate` — in production the daemon-ownership verdict (`AgentController.live`
  wires it; `gateFor` allows only `unmanaged`/`selfManaged`, denies `systemManaged`/
  `awaitingApproval`/`indeterminate`). It returns its verdict rather than mapping
  errors itself — a `register` throw is a failed *install*, a `kickstart` throw a
  failed *start* with the agent still installed, so the two never bleed into one
  shared state. Never call `SMAppService`/`launchctl` from a view action; that
  bypasses the gate. The default `spawnGate` stays `.allow` so the existing unit
  tests are unaffected; the ownership gather is injected (systemProbe/home) so the
  verdict is tested with fakes.
- **Ownership is three facts, in a load-bearing order.** `classify` reduces
  `homeIsCanonical` + the app's `SMAppService` self-status + a `system/`-domain
  canonical-label probe to one of five verdicts, in this order: a non-canonical home
  defers FIRST (a can't-happen override guard); a registered system daemon wins next
  (`systemManaged` — it owns the shared socket the app dials, ahead of our own
  agent); a wedged system probe then fails CLOSED (`indeterminate`); finally the
  self-status gives the per-user agent's own life stage (`.installed` →
  `selfManaged`, `.requiresApproval` → `awaitingApproval`, `.registrationFailed` →
  `indeterminate`, else `unmanaged`). The fail-closed system probe is checked BEFORE
  self-status, so a wedged probe defers even when our agent reads `.enabled` — if a
  system daemon is in fact present it outranks the per-user agent, and
  managing/reinstalling over it is the orphaned-per-user stomp. (This is the one
  deliberate behavior change from the older model, which let self-identity override a
  wedged probe.)
- **The shared label is disambiguated by self-status, never a label match.** The
  canonical label `com.coderinserepeat.runnyd` is shared by the app's own per-user
  agent AND the system daemon (they differ only by launchd DOMAIN — `gui/` vs
  `system/`), so a registered label cannot tell "mine" from "theirs". `.enabled`
  (`.installed`) self-status is what marks the per-user agent ours, sound only
  because a system/foreign `launchctl bootstrap` never flips the app's SMAppService
  status to `.enabled` (the registration databases are separate; see
  [ADR-0019](../../../../docs/architecture-decisions/0019-daemon-ownership-detection.md)).
  Do NOT "simplify" detection to a label comparison — that reintroduces the stomp.
- **The `system/`-domain probe is the ONE foreign-detection axis.** `gatherInputs`
  probes the canonical label in `system/`, and a registered hit is `systemManaged` —
  the app observes the daemon over the shared socket and never installs a competing
  per-user agent (`gateFor`/`startAffordance` treat it as not-ours; the observer
  banner names it). A registered system daemon is the only stomp the per-user path
  can't auto-heal (a per-user agent installed over a live system daemon runs orphaned
  while clients keep resolving the system home); every other contender — a hand-run
  dev daemon, a leftover brew/manual install — converges via the single-instance
  `flock` and is deliberately NOT detected (it reads `unmanaged`). The probe is
  read-only observation: the app never installs or removes the system daemon (that
  is `runnyctl`'s; the app is non-privileged — ADR-0023).
- **The probe is stdout-literal-match, bounded, and reaped.** `LaunchdProbe` runs
  `launchctl print system/<label>` and decides registration by the literal label
  appearing in byte-capped STDOUT — never exit code, never format parsing. A
  non-root user CAN read the `system/` domain (registered → label in stdout, absent →
  "could not find … in domain for system"). A registered job carries the label in
  stdout running OR stopped, and an absent one echoes it only in STDERR, so a
  combined-stream search would false-positive — search stdout only. On timeout it
  SIGTERMs then SIGKILLs after a grace with a detached reaper and explicit FD close,
  so a wedged launchctl yields `.indeterminate` without leaking a process or FDs.
- **The Start recovery bound is healthy-magnitude × margin, not a budget sum.**
  `startRecoveryBound` is sized to a normal cold start, and recovery is confirmed
  from a later `.connected` snapshot — never the `kickstart` return. On expiry it
  is loud (`didNotComeUp`), never a silent spinner.
