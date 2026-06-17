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
  surfaces "couldn't determine agent state" loudly, never spins.
