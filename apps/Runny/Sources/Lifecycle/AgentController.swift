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

    private let registrar: ServiceRegistrar
    private let spawnGate: () async -> SpawnGate
    private let eligibilityProvider: () -> LaunchAgentStatus.Eligibility

    init(
        registrar: ServiceRegistrar,
        spawnGate: @escaping () async -> SpawnGate = { .allow },
        eligibility: @escaping () -> LaunchAgentStatus.Eligibility = { AgentController.bundleEligibility() }
    ) {
        self.registrar = registrar
        self.spawnGate = spawnGate
        eligibilityProvider = eligibility
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
        await attemptSpawn("install") { try self.registrar.register() }
    }

    /// The start-at-login toggle's ON position. Identical to `install()` — there
    /// is no separate "enabled but not installed" state for a bundled agent.
    func enableStartAtLogin() async { await install() }

    // MARK: - Teardown (NOT spawn-triggering — no gate)

    /// Uninstall: `unregister()` THEN a best-effort `bootout` (ordered so the
    /// bootout's "No such process" is the expected success, since unregister may
    /// already have removed the job). A unregister throw or a real bootout failure
    /// is surfaced loud, never swallowed.
    func uninstall() async {
        do {
            try registrar.unregister()
        } catch {
            installState = .registrationFailed(reason: "unregister failed: \(error.localizedDescription)")
            return
        }
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

    /// Every spawn-triggering action runs through here. Consults `spawnGate` and,
    /// on `.deny`, aborts LOUDLY — records the refusal and does NOT call the
    /// registrar (no spawn). On `.allow` it runs the action and re-derives
    /// `installState` from status, mapping a throw to `registrationFailed`. A
    /// direct `SMAppService`/launchctl call in a view action — bypassing this — is
    /// the anti-pattern the seam exists to prevent; P5 fills the gate here.
    private func attemptSpawn(_ label: String, _ body: () throws -> Void) async {
        if case let .deny(reason) = await spawnGate() {
            spawnRefusal = "\(label) was not started: \(reason)"
            return
        }
        spawnRefusal = nil
        do {
            try body()
        } catch {
            installState = .registrationFailed(reason: "\(label) failed: \(error.localizedDescription)")
            return
        }
        refresh()
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
}
