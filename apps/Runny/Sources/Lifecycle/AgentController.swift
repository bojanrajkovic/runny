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

    private var activationObserver: NSObjectProtocol?

    init(
        registrar: ServiceRegistrar,
        spawnGate: @escaping () async -> SpawnGate = { .allow },
        eligibility: @escaping () -> LaunchAgentStatus.Eligibility = { AgentController.bundleEligibility() }
    ) {
        self.registrar = registrar
        self.spawnGate = spawnGate
        eligibilityProvider = eligibility
        // Re-read the install status when the app returns to the foreground — e.g.
        // after the user enabled the agent in System Settings via the Login Items
        // CTA — so an already-open window doesn't stay stale at .requiresApproval
        // until it is reopened. Cheap (an SMAppService status read), so it's fine on
        // every activation; reconcile is left to the per-surface appear.
        // [weak self] + app-lifetime AgentController: no explicit removal needed (a
        // nonisolated deinit can't touch the MainActor property anyway, and the
        // controller never deinits — it's a @State for the app's whole run).
        activationObserver = NotificationCenter.default.addObserver(
            forName: NSApplication.didBecomeActiveNotification, object: nil, queue: .main
        ) { [weak self] _ in
            MainActor.assumeIsolated { self?.refresh() }
        }
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
            translocated: CLIInstallModel.isTranslocated(path)
        )
    }

    /// Recompute `installState` from the registrar's status. Called on appear and
    /// after every op — the single place a status maps to state.
    func refresh() {
        installState = LaunchAgentStatus.state(from: registrar.status())
    }

    // MARK: - Spawn-triggering actions (every one funnels through attemptSpawn)

    /// Install = register the agent. RunAtLoad means registering it IS enabling
    /// start-at-login, so the toggle's enable path and install are one action.
    func install() async {
        switch await attemptSpawn("install", { try self.registrar.register() }) {
        case .ran:
            refresh()
            reconcileState = .notChecked // the registration changed — re-check on next appear
        case .denied: break // spawnRefusal already set; installState unchanged
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

    /// Repair a foreign/stale-path agent by re-registering the canonical agent,
    /// which re-points the SMAppService job's bundle-relative program to this
    /// bundle, then re-reconciling to self-verify the re-point actually took.
    /// Funnels through the spawn chokepoint exactly like `install()`. Only
    /// meaningful from a canonical-eligible bundle — re-registering elsewhere would
    /// install ANOTHER non-canonical agent — so the surface gates the action on
    /// `canRepair`, the same way it gates install on `canToggle`, AND raises a
    /// confirmation that warns about displacing a foreign manager (the spawn gate
    /// is `.allow` until detect-and-defer lands, so the consent is the guard). If
    /// the re-point does not take (a foreign MANAGER still owns the label), the
    /// re-run reconcile honestly keeps showing foreign rather than a false
    /// all-clear off the register return.
    func repair() async {
        repairError = nil
        switch await attemptSpawn("repair", { try self.registrar.register() }) {
        case .ran:
            refresh()
            await runReconcile()
        case .denied:
            // The gate blocked the re-register. installState/reconcileState are
            // unchanged, so without surfacing this the warning + button would just
            // silently persist — surface the refusal loudly in the row.
            repairError = spawnRefusal
        case let .failed(error):
            // A re-register throw does NOT unregister the existing (foreign) agent,
            // so keep installState derived from status — the uninstall path must
            // survive — and surface the repair error separately.
            refresh()
            repairError = "repair failed: \(error.localizedDescription)"
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
