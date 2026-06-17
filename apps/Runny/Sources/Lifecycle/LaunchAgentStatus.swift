import Foundation
import ServiceManagement

/// The lifecycle module's pure decision layer: `SMAppService.Status` → a closed,
/// loud install state, plus the install-location and reconcile verdicts. No live
/// `SMAppService`/`launchctl` calls live here — `AgentController` owns those and
/// feeds these functions plain values, so every branch is unit-testable without
/// launchd. Mirrors `DaemonStore`'s pure-static-verdict + thin-side-effect split.
enum LaunchAgentStatus {
    /// The app's canonical install location. SMAppService can only register an
    /// agent from a stable bundle path, and this is the one runny supports.
    /// Deliberately NOT `Bundle.main.bundlePath`: that is the transient
    /// translocation mount on a `~/Downloads` launch, which would make the
    /// reconcile flag a perfectly-good `/Applications` agent as foreign.
    static let canonicalBundlePath = "/Applications/Runny.app"

    /// The agent's program path when installed canonically — what a registered
    /// job's bundle-relative `BundleProgram` resolves to. The reconcile compares
    /// an observed program path against THIS.
    static var canonicalAgentProgram: String {
        canonicalBundlePath + "/Contents/MacOS/runnyd"
    }

    /// A closed, loud set. Every `SMAppService.Status` maps to a named case, and
    /// a `register()`/`unregister()` THROW becomes `registrationFailed` — never a
    /// silent fall-through to `notInstalled`. `requiresApproval` is a first-class
    /// CTA (open Login Items), not a disguised failure.
    enum State: Equatable {
        case notInstalled
        case installed
        case requiresApproval
        case notFound
        case registrationFailed(reason: String)
    }

    /// Map the raw SMAppService status. The throw path (register/unregister) is
    /// produced by `AgentController`, not here. A future status we do not model is
    /// a determination FAILURE surfaced loud, never silently rendered installed
    /// or not-installed.
    nonisolated static func state(from status: SMAppService.Status) -> State {
        switch status {
        case .notRegistered: .notInstalled
        case .enabled: .installed
        case .requiresApproval: .requiresApproval
        case .notFound: .notFound
        @unknown default:
            .registrationFailed(reason: "unrecognized SMAppService status (rawValue \(status.rawValue))")
        }
    }

    /// Whether the app may install its agent from where it is currently running.
    enum Eligibility: Equatable {
        /// In `/Applications` and not translocated — install allowed.
        case eligible
        /// Running from a translocation mount: Gatekeeper first-launch can
        /// transiently translocate even a correctly-installed `/Applications`
        /// app, and a `~/Downloads`/dmg launch always does. RECOVERABLE, never a
        /// permanent refusal — re-launching from `/Applications` clears it. This
        /// is the "first-launch quarantine → re-launch and retry" case.
        case translocated
        /// Not translocated but not in `/Applications` (a dev build run in place,
        /// an `~/Applications` copy). RECOVERABLE: move to `/Applications`.
        case notInApplications(path: String)
    }

    /// Translocation is checked FIRST: a translocated bundle's path is the
    /// transient mount, so the `/Applications` comparison cannot be trusted there.
    /// The translocated verdict is recoverable, so a correctly-installed app that
    /// is transiently translocated on its very first launch is never permanently
    /// refused — the silent-failure the §5 analysis guards against.
    nonisolated static func eligibility(bundlePath: String, translocated: Bool) -> Eligibility {
        if translocated { return .translocated }
        if bundlePath == canonicalBundlePath { return .eligible }
        return .notInApplications(path: bundlePath)
    }

    /// Whether the start-at-login toggle is actionable. Eligibility gates only
    /// turning it ON (install needs a stable `/Applications` location); a
    /// registered agent stays toggle-able regardless of location so a stale or
    /// unwanted agent can always be uninstalled — including from the translocated /
    /// moved-out-of-Applications recovery cases the row reports.
    nonisolated static func canToggle(state: State, eligibility: Eligibility) -> Bool {
        switch state {
        case .installed, .requiresApproval: true // registered → allow uninstall anywhere
        case .notFound: false // no bundled plist to register or remove
        case .notInstalled, .registrationFailed: eligibility == .eligible // install gate
        }
    }

    /// A registered agent is foreign/stale when its program path is not THIS
    /// app's canonical agent program. Compared against `canonicalAgentProgram`,
    /// never the running bundle's path — a `/Applications` agent observed from a
    /// translocated `~/Downloads` launch is good, not stale.
    nonisolated static func isCanonicalAgentProgram(_ programPath: String) -> Bool {
        programPath == canonicalAgentProgram
    }

    /// What the daemon-not-running surface should offer.
    enum StartAffordance: Equatable {
        /// Not installed, or the daemon is reachable — offer nothing.
        case none
        /// Installed but the daemon is unreachable — offer Start (kickstart).
        case start
        /// Registered but Login Items approval is pending — offer the approval CTA,
        /// NEVER a Start that would kickstart a job launchd refuses to run.
        case approval
    }

    /// The Start gate. Start is offered ONLY when the agent is installed AND the
    /// daemon is unreachable; `requiresApproval` is its own approval CTA, so a dead
    /// Start button can never no-op against a job launchd won't run.
    nonisolated static func startAffordance(state: State, daemonUnreachable: Bool) -> StartAffordance {
        switch state {
        case .requiresApproval: .approval
        case .installed: daemonUnreachable ? .start : .none
        case .notInstalled, .notFound, .registrationFailed: .none
        }
    }
}
