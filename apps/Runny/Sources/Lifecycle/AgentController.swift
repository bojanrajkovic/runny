import AppKit
import Foundation
import Observation
import ServiceManagement

/// The decision P5 fills with the foreign-manager ownership verdict. P4 ships the
/// gate defaulting to `.allow`; every spawn-triggering action funnels through
/// `attemptSpawn(_:)`, which consults it, so P5 injects one verdict without
/// touching a call site. Async so P5's detection (which may introspect launchd)
/// fits without reshaping the seam.
enum SpawnGate: Equatable {
    case allow
    case deny(reason: String)
}

/// The result of the best-effort `launchctl bootout` on uninstall.
enum BootoutOutcome: Equatable {
    /// launchctl removed the job.
    case removed
    /// "No such process" / not loaded — `unregister()` already tore it down. This
    /// is SUCCESS for uninstall, not a failure.
    case notLoaded
    /// launchctl reported a real error.
    case failed(String)
    /// launchctl did not respond within the bound — surfaced loud, never a spin.
    case timedOut
}

/// What the LaunchAgent lifecycle needs from `SMAppService`/launchctl, behind a
/// protocol so `AgentController`'s decisions are testable against a mock without a
/// registered bundle (SMAppService cannot be exercised in a unit test). The real
/// conformer, `SMAppServiceRegistrar`, is the ONLY place those side effects live.
///
/// `status()`/`register()`/`unregister()` are synchronous SMAppService XPC calls;
/// a throw from register/unregister means launchd REJECTED the request (SIP/MDM,
/// etc.), never that the daemon is up. `bootout()` runs a launchctl subprocess and
/// is async + internally bounded.
@MainActor
protocol ServiceRegistrar {
    func status() -> SMAppService.Status
    func register() throws
    func unregister() throws
    func bootout() async -> BootoutOutcome
    /// Start a registered-but-stopped agent (`launchctl kickstart`, no `-k` — never
    /// a SIGKILL of a running daemon). Throws on failure/timeout; the daemon coming
    /// up is confirmed from the connection, not this call's return.
    func kickstart() async throws
    /// Read the registered agent's resolved program path (bounded launchctl
    /// introspection), for the reconcile. Never hangs — times out to `.undetermined`.
    func agentProgramPath() async -> AgentProgram
}

/// The registered agent's program, read by launchctl introspection for the
/// reconcile. `undetermined` covers a timeout or unparseable output — surfaced as
/// "couldn't determine", never a spin or a false "foreign".
enum AgentProgram: Equatable {
    case program(String)
    case notRegistered
    case undetermined
}

/// Whether the agent registered under our label points where it should. Compared
/// against the CANONICAL `/Applications/Runny.app`, never the running bundle — a
/// good `/Applications` agent observed from a translocated `~/Downloads` launch is
/// not stale.
enum AgentReconcile: Equatable {
    /// Reconcile has not run on this surface yet — explicitly NOT a canonical
    /// confirmation, so the update affordance (which requires affirmative `.ok`)
    /// stays hidden until a real verdict lands.
    case notChecked
    /// Canonical, or no agent registered — nothing to surface.
    case ok
    /// A runnyd agent is registered from a non-canonical program path.
    case foreign(path: String)
    /// launchctl introspection timed out or didn't parse — surfaced, not alarmed.
    case undetermined
}

/// The outcome of a Start affordance: the daemon coming up is confirmed from a
/// later `.connected` snapshot within a bound, never from the kickstart return.
enum StartOutcome: Equatable {
    case idle
    case starting
    case cameUp
    /// kickstart issued, but no `.connected` within the recovery bound — surfaced
    /// loud ("Start issued but the daemon hasn't come up"), never a silent spinner.
    case didNotComeUp
    /// The gate denied the start, or kickstart itself failed.
    case refused(String)
}

/// What the observer banner shows when the app is NOT the daemon's manager. The
/// `kind` is read for styling/icon and so a surface can branch on the verdict
/// without string-matching the prose; the `message` names the managing channel
/// and the operator's next step. nil for `unmanaged`/`selfManaged`/`awaitingApproval`
/// — those are the app's own install/approval UI, not an observer posture.
struct ObserverHint: Equatable {
    enum Kind: Equatable {
        case systemManaged
        case indeterminate
    }

    let kind: Kind
    let message: String
}

/// The thin side-effect wrapper over `SMAppService.agent`, exposing a published
/// `installState` (the closed, loud `LaunchAgentStatus.State`) and the
/// spawn-gated install/uninstall actions every surface drives. Decisions are the
/// pure `LaunchAgentStatus` verdicts; this file owns only the orchestration and
/// the requested-vs-done discipline.
@MainActor
@Observable
final class AgentController {
    /// Derived from `service.status`, never from a call's return — a `register()`
    /// that returned without throwing means *requested*, and `installState`
    /// reflects that only after re-reading the status.
    private(set) var installState: LaunchAgentStatus.State = .notInstalled

    /// The last spawn-gate refusal (P5 fills the gate; P4 ships `.allow`, so this
    /// stays nil in P4). Surfaced loud, never a silent no-op.
    private(set) var spawnRefusal: String?

    /// The Start affordance's progress/outcome. Distinct from `installState`: a
    /// failed start leaves the agent installed, so it must not flip the install
    /// state to failed.
    private(set) var startOutcome: StartOutcome = .idle

    /// Guards against a second `start()` stacking a concurrent recovery poll while
    /// one is already running. Reset on every exit (including cancellation).
    private var startInFlight = false

    /// How long to wait for the daemon to answer after a kickstart before
    /// surfacing "didn't come up". A healthy cold start is NOT just the launchd
    /// spawn — the socket opens only AFTER startup validation (the GitHub
    /// permission check + a sequential image-resolve per pool), so on a normal
    /// config that healthy magnitude is several seconds, more for a large fleet.
    /// This bound is that healthy magnitude × margin — deliberately NOT the sum of
    /// the daemon's per-check budgets (the degraded envelope). And it self-corrects:
    /// `didNotComeUp` clears the instant the daemon's own stream connects, so a
    /// genuinely slow-but-healthy start surfaces only a transient message.
    static let startRecoveryBound: Duration = .seconds(60)

    private let registrar: ServiceRegistrar
    private let spawnGate: () async -> SpawnGate
    private let eligibilityProvider: () -> LaunchAgentStatus.Eligibility
    private let bundledAgentPresentProvider: () -> Bool
    private let systemProbeProvider: @Sendable (String) async -> LaunchdProbeResult
    private let homeIsCanonicalProvider: () -> Bool

    private var activationObserver: NSObjectProtocol?

    init(
        registrar: ServiceRegistrar,
        spawnGate: @escaping () async -> SpawnGate = { .allow },
        eligibility: @escaping () -> LaunchAgentStatus.Eligibility = { AgentController.bundleEligibility() },
        bundledAgentPresent: @escaping () -> Bool = { AgentController.bundledAgentPresent() },
        // Hermetic by default: existing tests construct AgentController without knowing
        // about systemProbe, so its default must NOT shell out to `launchctl print
        // system/…`. `live()` supplies the real probe.
        systemProbe: @escaping @Sendable (String) async -> LaunchdProbeResult = { _ in .notRegistered },
        homeIsCanonical: @escaping () -> Bool = { true }
    ) {
        self.registrar = registrar
        self.spawnGate = spawnGate
        eligibilityProvider = eligibility
        bundledAgentPresentProvider = bundledAgentPresent
        systemProbeProvider = systemProbe
        homeIsCanonicalProvider = homeIsCanonical
        // Re-read the install status AND the ownership verdict when the app returns
        // to the foreground — e.g. after the user enabled the agent in System
        // Settings via the Login Items CTA, or a system daemon was installed while the
        // app was idle — so an already-open window doesn't stay stale. The status read
        // is cheap; the ownership refresh runs the bounded probe off the main actor.
        // [weak self] + app-lifetime AgentController: no explicit removal needed (a
        // nonisolated deinit can't touch the MainActor property anyway, and the
        // controller never deinits — it's a @State for the app's whole run).
        activationObserver = NotificationCenter.default.addObserver(
            forName: NSApplication.didBecomeActiveNotification, object: nil, queue: .main
        ) { [weak self] _ in
            MainActor.assumeIsolated {
                self?.refresh()
                Task { await self?.refreshOwnership() }
            }
        }
    }

    /// The production controller: a real `SMAppServiceRegistrar`, and a spawn gate
    /// that gathers the live ownership verdict and denies install/repair/start for a
    /// `systemManaged` or `indeterminate` owner. The same gather feeds
    /// `refreshOwnership` (the observer banner), so the gate and the UI never
    /// disagree. The one foreign-detection axis is the `system/`-domain canonical
    /// label probe — a registered system daemon is the only stomp the per-user path
    /// can't auto-heal, and a wedged probe fails closed.
    @MainActor
    static func live() -> AgentController {
        let registrar = SMAppServiceRegistrar()
        let systemProbe: @Sendable (String) async -> LaunchdProbeResult = {
            await LaunchdProbe.probe(label: $0)
        }
        let homeIsCanonical = { true }
        let bundledAgentPresent = { AgentController.bundledAgentPresent() }
        return AgentController(
            registrar: registrar,
            spawnGate: {
                await gateFor(DaemonOwnership.classify(gatherInputs(
                    registrar: registrar, systemProbe: systemProbe,
                    homeIsCanonical: homeIsCanonical, bundledAgentPresent: bundledAgentPresent
                )))
            },
            bundledAgentPresent: bundledAgentPresent,
            systemProbe: systemProbe,
            homeIsCanonical: homeIsCanonical
        )
    }

    /// Whether this running bundle may install its agent — translocated, in
    /// `/Applications`, or somewhere else. Read live (the bundle's location and
    /// translocation are fixed for a launch, but the surface re-reads on appear).
    var eligibility: LaunchAgentStatus.Eligibility { eligibilityProvider() }

    /// The real eligibility read: this bundle's location and translocation. Reuses
    /// the one translocation heuristic in `CLIInstallModel` (no drifting copy of a
    /// safety check), and strips a trailing slash so a directory-URL path still
    /// matches the canonical `/Applications/Runny.app`.
    nonisolated static func bundleEligibility() -> LaunchAgentStatus.Eligibility {
        var path = Bundle.main.bundleURL.path
        if path.count > 1, path.hasSuffix("/") { path.removeLast() }
        return LaunchAgentStatus.eligibility(
            bundlePath: path,
            translocated: PrivilegedBroker.isTranslocated(path)
        )
    }

    /// Whether the LaunchAgent plist is actually present in the running bundle at
    /// the SMAppService-resolved `Contents/Library/LaunchAgents/<plistName>`. A
    /// release carries it (injected before signing); a bare `bazel run` dev build
    /// does not. This is the independent fact that disambiguates SMAppService's
    /// overloaded `.notFound` — never-registered-but-bundled (installable) vs.
    /// genuinely-no-agent (a dev build) — which the status alone cannot tell apart.
    nonisolated static func bundledAgentPresent() -> Bool {
        let url = Bundle.main.bundleURL
            .appendingPathComponent("Contents/Library/LaunchAgents")
            .appendingPathComponent(SMAppServiceRegistrar.plistName)
        return FileManager.default.fileExists(atPath: url.path)
    }

    /// Recompute `installState` from the registrar's status. Called on appear and
    /// after every op — the single place a status maps to state. Passes whether the
    /// plist is actually bundled so the overloaded `.notFound` resolves correctly:
    /// a never-registered release agent is installable, a no-plist dev build is not.
    func refresh() {
        installState = LaunchAgentStatus.state(
            from: registrar.status(), bundledAgentPresent: bundledAgentPresentProvider()
        )
    }

    /// Who manages the daemon, refreshed on app-foreground and (freshly, by the
    /// production spawn gate) before any spawn. Read by the observer banner to name
    /// the managing channel. Starts `.indeterminate` (defer until a real gather
    /// runs); the gate never trusts this stale value — it gathers fresh per attempt.
    private(set) var ownership: DaemonOwnership = .indeterminate

    /// Whether a real gather has run yet. The initial `.indeterminate` is "not
    /// checked", not a verdict — the banner shows "Checking…" until this flips, so a
    /// pristine launch never flashes the indeterminate diagnostic before the probes
    /// have run.
    private(set) var ownershipChecked = false

    private var ownershipInFlight = false
    private var ownershipPending = false

    /// Gather the ownership inputs and publish the verdict (for the banner and the
    /// install gate). The gate's pre-act recheck calls `gatherInputs` directly and
    /// classifies for a fresh verdict. Coalesces overlapping triggers — the activation
    /// observer and a surface's `.task` can both fire on one foreground — so a trigger
    /// arriving mid-gather loops the active run once more against the latest state
    /// rather than spawning a second concurrent probe and racing last-writer-wins to
    /// publish.
    func refreshOwnership() async {
        if ownershipInFlight {
            ownershipPending = true
            return
        }
        ownershipInFlight = true
        defer { ownershipInFlight = false }
        repeat {
            ownershipPending = false
            let inputs = await Self.gatherInputs(
                registrar: registrar, systemProbe: systemProbeProvider,
                homeIsCanonical: homeIsCanonicalProvider, bundledAgentPresent: bundledAgentPresentProvider
            )
            ownership = DaemonOwnership.classify(inputs)
            ownershipChecked = true
        } while ownershipPending
    }

    /// Re-gather ownership and report whether it still matches `expected` — for the
    /// daemon-affecting actions that fire OUTSIDE the spawn gate (the approval CTA that
    /// opens Login Items, the drain-gated Update) and so can't trust the render-time
    /// verdict. The spawn gate re-gathers per attempt; these must too, or a foreign owner
    /// that appeared while the window stayed open lets the stale CTA fire over it
    /// (approving a competing agent, or draining a now-foreign fleet). Publishing the
    /// fresh verdict also flips the surface to the observer banner when it returns false.
    func revalidate(_ expected: DaemonOwnership) async -> Bool {
        await refreshOwnership()
        return ownership == expected
    }

    /// Gather the three facts `classify` reduces to a verdict: the `system/`-domain
    /// canonical-label probe (the one foreign-detection axis), the app's own
    /// SMAppService self-status, and whether the home is canonical. Static so both
    /// `refreshOwnership` and the production gate share one gather without a
    /// self-reference cycle. `systemProbe` has no default: a defaulted "no system
    /// probe" would be exactly the silent-skip (install over a system daemon) this
    /// exists to prevent.
    @MainActor
    static func gatherInputs(
        registrar: ServiceRegistrar,
        systemProbe: @Sendable (String) async -> LaunchdProbeResult,
        homeIsCanonical: () -> Bool,
        bundledAgentPresent: () -> Bool
    ) async -> DaemonOwnershipInputs {
        let system = await systemProbe(DaemonOwnership.canonicalLabel)
        let selfState = LaunchAgentStatus.state(
            from: registrar.status(), bundledAgentPresent: bundledAgentPresent()
        )
        return DaemonOwnershipInputs(
            homeIsCanonical: homeIsCanonical(),
            selfState: selfState,
            systemProbe: system
        )
    }

    // MARK: - Spawn-triggering actions (every one funnels through attemptSpawn)

    /// Install = register the agent. RunAtLoad means registering it IS enabling
    /// start-at-login, so the toggle's enable path and install are one action.
    func install() async {
        switch await attemptSpawn("install", { try self.registrar.register() }) {
        case .ran:
            refresh()
            reconcileState = .notChecked // the registration changed — re-check on next appear
            // Re-gather ownership too: the affordances gate on the verdict (Start needs
            // selfManaged, the approval CTA awaitingApproval, Update selfManaged), so
            // without this the just-installed agent stays the pre-install `.unmanaged`
            // verdict until the next app activation — suppressing the very controls the
            // install just enabled.
            await refreshOwnership()
        case .denied:
            // A foreign manager appeared since the last refresh, so the fresh pre-act
            // gate denied. Publish the new verdict so the observer banner replaces the
            // toggle — otherwise a dead Install button silently no-ops on every click.
            await refreshOwnership()
        case let .failed(error):
            installState = .registrationFailed(reason: "install failed: \(error.localizedDescription)")
        }
    }

    /// A later live connection proved the daemon recovered — clear a TERMINAL Start
    /// outcome so a subsequent unrelated outage shows a fresh Start, not the stale
    /// "Start issued"/"Try Again" from a prior attempt. Leaves an in-flight
    /// `.starting` alone (start()'s own poll owns that).
    func noteRecovered() {
        switch startOutcome {
        case .didNotComeUp, .refused, .cameUp: startOutcome = .idle
        case .idle, .starting: break
        }
    }

    /// Start a registered-but-stopped daemon (the menu/window Start affordance, and
    /// the kickstart fallback). Funnels through the gate, then confirms the daemon
    /// actually came up from a later `.connected` snapshot within the recovery
    /// bound — never from the kickstart return. On expiry it surfaces
    /// `didNotComeUp` loudly. `isConnected` is the live connection read (injected so
    /// the controller need not own the daemon stream); `within`/`poll` are
    /// overridable for tests.
    func start(
        isConnected: @escaping () -> Bool,
        within bound: Duration = AgentController.startRecoveryBound,
        poll: Duration = .milliseconds(500)
    ) async {
        guard !startInFlight else { return } // a recovery poll is already running
        startInFlight = true
        defer { startInFlight = false }
        startOutcome = .starting
        switch await attemptSpawn("start", { try await self.registrar.kickstart() }) {
        case .denied:
            startOutcome = .refused(spawnRefusal ?? "start was blocked")
            // Publish the fresh foreign verdict the gate just gathered (as install()'s
            // denied path does), so the Start row gives way to the observer banner —
            // otherwise the stale .selfManaged keeps a Start button the gate re-denies on
            // every Try Again until the next app activation.
            await refreshOwnership()
            return
        case let .failed(error):
            startOutcome = .refused("could not start runnyd: \(error.localizedDescription)")
            return
        case .ran:
            break
        }
        // Confirm recovery from the connection, never the kickstart return.
        let deadline = ContinuousClock.now.advanced(by: bound)
        while ContinuousClock.now < deadline {
            if isConnected() {
                startOutcome = .cameUp
                return
            }
            do {
                try await Task.sleep(for: poll)
            } catch {
                // Cancelled (the surface went away): stop the poll and clear the
                // spinner rather than busy-spinning to the deadline.
                startOutcome = .idle
                return
            }
        }
        startOutcome = isConnected() ? .cameUp : .didNotComeUp
    }

    /// The last reconcile verdict — whether the registered agent points where it
    /// should. Defaults to `.notChecked` (not canonical), so a surface that gates on
    /// `.ok` shows nothing until reconcile actually runs. A `.foreign` verdict is
    /// repairable in place from a canonical bundle via `repair()`.
    private(set) var reconcileState: AgentReconcile = .notChecked
    private var reconcileInFlight = false
    private var reconcilePending = false

    /// A repair-specific failure or denial message, surfaced in the reconcile
    /// warning row. Distinct from `installState`: a failed or denied repair leaves
    /// the existing (foreign) agent registered, so the failure must NOT masquerade
    /// as an install failure that flips the toggle off and drops the uninstall path.
    private(set) var repairError: String?

    /// Reconcile-on-launch: read the registered agent's program path and compare it
    /// to the canonical install location. Surfaces a foreign/stale-path agent, and
    /// an introspection that times out as "couldn't determine" — never a spin.
    ///
    /// Coalesces rather than drops: a trigger arriving while a read is in flight
    /// (another surface appearing, or repair's self-verify) sets `reconcilePending`
    /// so the active run loops once more and publishes a verdict against the LATEST
    /// registration. The plain guard would let a stale pre-repair read win and
    /// leave a successful repair reported as foreign until the next appear; it also
    /// still prevents two launchctl subprocesses from racing concurrently.
    func runReconcile() async {
        if reconcileInFlight {
            reconcilePending = true
            return
        }
        reconcileInFlight = true
        defer { reconcileInFlight = false }
        repeat {
            reconcilePending = false
            reconcileState = await Self.reconcileVerdict(registrar.agentProgramPath())
        } while reconcilePending
    }

    /// Repair a stale-path self-managed agent by a verified unregister→register
    /// replace: `unregister()` clears the stale registration, then `register()`
    /// re-adds it pointing at this (canonical) bundle. A bare `register()` on an
    /// already-registered agent is NOT a reliable re-point — some macOS versions
    /// return already-registered without updating the program path — so the replace
    /// is what actually moves the program. Re-reconciles afterward to self-verify
    /// the re-point took, rather than trusting the call's return.
    ///
    /// Only the daemon's own stale agent is reached: the spawn gate denies every
    /// foreign/indeterminate verdict, and the observer banner replaces the install
    /// section (which hosts the Repair button) for a foreign owner — so a
    /// brew/manual daemon is never reached here, let alone unregistered. The surface
    /// also gates the button on `canRepair` (a canonical-eligible bundle only;
    /// re-registering elsewhere would install ANOTHER non-canonical agent).
    ///
    /// The replace waits for BOTH steps to actually take — an SMAppService call
    /// returning means launchd ACCEPTED the request, not that it finished. It waits for
    /// the unregister so the re-register can't race a still-converging removal into an
    /// already-registered no-op, AND for the re-register (`awaitRegistered`) so the
    /// self-verifying reconcile doesn't read a still-converging registration as
    /// `.notRegistered` (→ a false `.ok` that clears the warning before the re-point
    /// lands). Bounded both ways: an unconfirmed removal OR an unconfirmed re-register
    /// aborts loud rather than reporting a false-verified repair. The surface raises the
    /// same live-guest abandon confirmation `uninstall()` uses before calling this, since
    /// `unregister()` can evict a running job on some macOS versions.
    ///
    /// Failure is honest: if `register()` throws after the `unregister()` took, the
    /// agent is genuinely gone, so installState (derived from status) reflects that
    /// and the toggle offers reinstall — recoverable and loud, never a silent stale
    /// agent. A denied gate never unregisters, so the foreign agent stays intact.
    func repair(
        within bound: Duration = AgentController.unregisterConfirmBound, poll: Duration = .milliseconds(100)
    ) async {
        repairError = nil
        switch await attemptSpawn("repair", {
            try self.registrar.unregister()
            try await self.awaitUnregistered(within: bound, poll: poll)
            try self.registrar.register()
            // Wait for the re-register to actually take before declaring success — a
            // register() that returned only means launchd ACCEPTED it. Without this, the
            // post-repair reconcile can read the still-converging agent as .notRegistered
            // → .ok and clear the warning while the re-point hasn't landed.
            try await self.awaitRegistered(within: bound, poll: poll)
        }) {
        case .ran:
            refresh()
            await runReconcile()
        case .denied:
            // The gate blocked the replace before any unregister — installState and
            // reconcileState are unchanged, so surface the refusal loudly. Publish the
            // fresh foreign verdict the gate gathered (as the install/start denied paths
            // do) so the observer banner replaces the Repair UI — otherwise the stale
            // selfManaged verdict keeps a Repair button the gate re-denies on every press.
            repairError = spawnRefusal
            await refreshOwnership()
        case let .failed(error):
            // unregister/register/the wait threw; installState derives from status
            // (gone if the unregister took and the register then failed), so the
            // uninstall/reinstall path stays honest. Surface the error, never silently.
            // Re-reconcile too: if the unregister took, the pre-repair .foreign verdict
            // is stale — leaving it would keep the row claiming an agent is registered at
            // the old path and offering a Repair that now just unregisters nothing.
            refresh()
            repairError = "repair failed: \(error.localizedDescription)"
            await runReconcile()
        }
    }

    /// How long to wait for `unregister()` to be reflected in status before giving up
    /// — a launchd removal is near-instant, so this is healthy-magnitude × margin.
    static let unregisterConfirmBound: Duration = .seconds(5)

    /// Poll the authoritative status until it confirms the agent is gone
    /// (`.notRegistered`/`.notFound`), bounded. Throws on expiry so repair aborts loud
    /// rather than re-registering over a still-registered agent.
    private func awaitUnregistered(within bound: Duration, poll: Duration) async throws {
        let deadline = ContinuousClock.now.advanced(by: bound)
        while true {
            switch registrar.status() {
            case .notRegistered, .notFound: return
            default: break
            }
            if ContinuousClock.now >= deadline {
                throw LaunchctlFailure(
                    message: "the existing agent did not unregister in time; aborted to avoid re-registering over it"
                )
            }
            try await Task.sleep(for: poll)
        }
    }

    /// Poll the authoritative status until it confirms the re-registered agent has
    /// appeared (`.enabled`/`.requiresApproval`), bounded — the symmetric partner of
    /// `awaitUnregistered`. `register()` returning means launchd ACCEPTED the request,
    /// not that it finished, so without this the post-repair reconcile can read the
    /// still-absent agent as `.notRegistered` (→ `.ok`) and clear the warning while the
    /// registration is still converging. Throws on expiry so repair aborts loud rather
    /// than reporting a false-verified re-point off a call that only requested it.
    private func awaitRegistered(within bound: Duration, poll: Duration) async throws {
        let deadline = ContinuousClock.now.advanced(by: bound)
        while true {
            switch registrar.status() {
            case .enabled, .requiresApproval: return
            default: break
            }
            if ContinuousClock.now >= deadline {
                throw LaunchctlFailure(
                    message: "the re-registered agent did not appear in time; aborted rather than report a false re-point"
                )
            }
            try await Task.sleep(for: poll)
        }
    }

    // MARK: - Teardown (NOT spawn-triggering — no gate)

    /// Uninstall: `unregister()` THEN a best-effort `bootout` (ordered so the
    /// bootout's "No such process" is the expected success, since unregister may
    /// already have removed the job). A unregister throw or a real bootout failure
    /// is surfaced loud, never swallowed.
    ///
    /// The explicit `bootout` is kept deliberately: it is not verified across the
    /// supported macOS versions whether `unregister()` alone evicts the *running*
    /// job, and dropping it on an OS that still needs it would silently leave the
    /// daemon running after an uninstall — the silent-failure this project refuses.
    /// Drop the explicit bootout only once it is proven redundant on every
    /// supported OS.
    func uninstall() async {
        do {
            try registrar.unregister()
        } catch {
            installState = .registrationFailed(reason: "unregister failed: \(error.localizedDescription)")
            return
        }
        // The registration is gone — clear any stale reconcile verdict so a reinstall
        // in the same session re-checks instead of staying hidden behind a dead .foreign.
        reconcileState = .notChecked
        switch await registrar.bootout() {
        case .removed, .notLoaded:
            refresh()
        case let .failed(msg):
            installState = .registrationFailed(reason: "agent unregistered but launchctl bootout failed: \(msg)")
        case .timedOut:
            installState = .registrationFailed(reason: "agent unregistered but launchctl bootout did not respond")
        }
    }

    // MARK: - The single spawn chokepoint

    /// The single gate every spawn-triggering action runs through. Consults
    /// `spawnGate` and, on `.deny`, records the refusal and returns `.denied`
    /// WITHOUT calling the registrar (no spawn). On `.allow` it runs the action and
    /// reports `.ran`/`.failed` — the per-action caller decides what state that
    /// maps to (a register throw is a failed install; a kickstart throw is a failed
    /// start, the agent still installed). A direct `SMAppService`/launchctl call in
    /// a view action — bypassing this — is the anti-pattern the seam prevents; P5
    /// fills the gate here.
    private func attemptSpawn(_ label: String, _ body: () async throws -> Void) async -> SpawnResult {
        if case let .deny(reason) = await spawnGate() {
            spawnRefusal = "\(label) was not started: \(reason)"
            return .denied
        }
        spawnRefusal = nil
        do {
            try await body()
            return .ran
        } catch {
            return .failed(error)
        }
    }

    /// The gate's verdict for one spawn attempt — kept loud and explicit so each
    /// caller maps a failure to its own surface rather than a shared default.
    private enum SpawnResult {
        case ran
        case denied
        case failed(Error)
    }
}

// MARK: - Pure mappers (nonisolated static, unit-tested)

extension AgentController {
    /// Classify `launchctl bootout`'s exit + stderr. A non-loaded job (exit 3 /
    /// "No such process") is the SUCCESS case on uninstall: `unregister()` already
    /// removed it, so there is nothing left to boot out. Pure → unit-tested.
    nonisolated static func classifyBootout(exitCode: Int32, stderr: String) -> BootoutOutcome {
        if exitCode == 0 { return .removed }
        let s = stderr.lowercased()
        if exitCode == 3 || s.contains("no such process") || s.contains("could not find") {
            return .notLoaded
        }
        return .failed(
            stderr.isEmpty
                ? "launchctl bootout exited \(exitCode)"
                : stderr.trimmingCharacters(in: .whitespacesAndNewlines)
        )
    }

    /// Reconcile verdict: compare the registered agent's program against the
    /// CANONICAL location. Pure → unit-tested. `notRegistered` and a canonical
    /// program are both `.ok`; a non-canonical program is `.foreign`.
    nonisolated static func reconcileVerdict(_ program: AgentProgram) -> AgentReconcile {
        switch program {
        case .notRegistered: .ok
        case .undetermined: .undetermined
        case let .program(path):
            LaunchAgentStatus.isCanonicalAgentProgram(path) ? .ok : .foreign(path: path)
        }
    }

    /// Whether to offer the in-app repair for a foreign/stale-path agent. Only a
    /// canonical-eligible bundle can repair by re-registering — from a translocated
    /// or non-`/Applications` bundle, re-registering would install ANOTHER
    /// non-canonical agent, so the surface shows move-to-`/Applications` guidance
    /// rather than a repair button. Pure → unit-tested.
    nonisolated static func canRepair(reconcile: AgentReconcile, eligibility: LaunchAgentStatus.Eligibility) -> Bool {
        if case .foreign = reconcile, eligibility == .eligible { return true }
        return false
    }

    /// Map an ownership verdict to a spawn-gate decision. Install/repair/start
    /// proceed only when the daemon is ours (`selfManaged`) or unowned
    /// (`unmanaged`); every foreign or indeterminate verdict denies and routes the
    /// surface to the observer banner — so the app never installs a second manager,
    /// never kicks a hand-run daemon, and never acts on an inconclusive probe. Pure
    /// → unit-tested. The `reason` is a short refusal; the banner carries the full
    /// channel-specific guidance.
    nonisolated static func gateFor(_ ownership: DaemonOwnership) -> SpawnGate {
        switch ownership {
        case .unmanaged, .selfManaged:
            .allow
        case .systemManaged:
            .deny(reason: "a system daemon manages runnyd on this host")
        case .awaitingApproval:
            .deny(reason: "the runnyd agent is registered and awaiting Login Items approval")
        case .indeterminate:
            .deny(reason: "couldn't determine who manages runnyd")
        }
    }

    /// The observer banner for a verdict that replaces the install toggle. `nil` for
    /// `unmanaged`/`selfManaged` (the install/toggle UI applies) and for
    /// `awaitingApproval` (the Login Items approval CTA, driven by `installState`,
    /// already covers it). Only `systemManaged` and `indeterminate` carry a banner.
    /// Pure → unit-tested wording.
    nonisolated static func observerMessage(for ownership: DaemonOwnership) -> ObserverHint? {
        switch ownership {
        case .unmanaged, .selfManaged, .awaitingApproval:
            nil
        case .systemManaged:
            // The app installs/removes the system daemon via the System Service
            // settings section (the brokered `runnyctl install-daemon`/`uninstall-daemon`),
            // so the banner names that as the management surface rather than framing the
            // daemon as foreign. Status still streams over the shared socket; the per-user
            // install toggle is hidden because a system daemon outranks a login agent.
            ObserverHint(
                kind: .systemManaged,
                message: "Runny manages this as a system-wide LaunchDaemon — it streams status here and "
                    + "won't install a competing login agent. Remove it in Settings → System Service."
            )
        case .indeterminate:
            ObserverHint(
                kind: .indeterminate,
                message: "Couldn't determine who manages runnyd, so Runny isn't installing. Check "
                    + "`launchctl print system/\(DaemonOwnership.canonicalLabel)` and reopen Runny."
            )
        }
    }

    /// Pure: pull the resolved program path out of `launchctl print` output, which
    /// prints a `program = /path` line for a loaded job. Defensive — returns nil if
    /// the line is absent (an unparseable/old format reconciles to undetermined,
    /// never a false foreign).
    nonisolated static func parseLaunchctlProgram(_ output: String) -> String? {
        for line in output.split(whereSeparator: \.isNewline) {
            let trimmed = line.trimmingCharacters(in: .whitespaces)
            if let range = trimmed.range(of: "program = "), range.lowerBound == trimmed.startIndex {
                let value = trimmed[range.upperBound...].trimmingCharacters(in: .whitespaces)
                return value.isEmpty ? nil : value
            }
        }
        return nil
    }
}
