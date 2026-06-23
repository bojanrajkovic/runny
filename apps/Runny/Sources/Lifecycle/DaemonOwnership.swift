import Foundation

/// Who manages the daemon, as a pure verdict over three facts the app gathers:
/// the resolved home's canonicity, the app's own SMAppService self-status, and
/// whether a system LaunchDaemon is registered. No side effects — `classify` is a
/// `nonisolated static` function over value types, unit-tested without a live
/// daemon or launchd. Mirrors `LaunchAgentStatus`'s pure-verdict shape; the impure
/// probe that produces `LaunchdProbeResult` lives in `LaunchdProbe`.
///
/// runnyd is supported in exactly two shapes: the app's per-user LaunchAgent (this
/// app, via SMAppService) and the installed system LaunchDaemon (`runnyctl
/// install-daemon`). The verdict set is those two shapes' life stages plus a
/// fail-closed diagnostic — three of the five (`unmanaged`/`awaitingApproval`/
/// `selfManaged`) are the SMAppService agent's own stages, `systemManaged` is the
/// other shape, and `indeterminate` defers when a fact can't be established. A
/// hand-run dev daemon isn't detected: it reads `unmanaged`, and the single-instance
/// `flock` makes installing over it converge harmlessly.
enum DaemonOwnership: Equatable {
    /// No manager owns the daemon — install is allowed.
    case unmanaged
    /// The app's own SMAppService agent owns it (self-status `.enabled`).
    case selfManaged
    /// Our own agent is registered but awaiting Login Items approval.
    case awaitingApproval
    /// A runnyd is registered in the `system/` launchd domain — the installed
    /// non-root system daemon. The app installs and removes it via the system path
    /// (`runnyctl install-daemon` / `uninstall-daemon`, brokered through the app or
    /// run directly), observes it over the shared socket, and never installs a
    /// competing per-user agent over it.
    case systemManaged
    /// A probe was inconclusive, the self-status was unrecognized, or the home is
    /// non-canonical: defer with a diagnostic — never install over what can't be
    /// ruled out.
    case indeterminate
}

/// The result of a single launchd-label probe (produced by `LaunchdProbe`). A
/// pure value so `classify` is testable without shelling out to `launchctl`.
enum LaunchdProbeResult: Equatable {
    case registered
    case notRegistered
    /// The probe wedged, timed out, or hit an ambiguous edge — treated as
    /// dominant by `classify` so uncertainty defers ahead of every positive branch.
    case indeterminate
}

/// The three orthogonal facts `classify` reduces to a verdict, gathered by the app
/// (`AgentController.gatherInputs`). Pure here.
struct DaemonOwnershipInputs: Equatable {
    /// Whether the resolved runny home is the canonical `~/.runny`. Always true now
    /// that the home is fixed (the override is gone); kept as a one-line
    /// defense-in-depth guard so a re-introduced override can never cause a
    /// cross-home stomp — and it defers FIRST.
    var homeIsCanonical: Bool
    /// The app's own agent status, already mapped (`.installed` == SMAppService
    /// `.enabled` == ours — a foreign owner never reads `.enabled`, so this is an
    /// authoritative self-identity signal).
    var selfState: LaunchAgentStatus.State
    /// Whether the canonical label is registered in the `system/` domain — the
    /// installed non-root system daemon (the headless / brokered deployment).
    /// Defaults `.notRegistered` so a host with no system daemon — the common case —
    /// reads as the per-user agent's own life stage.
    var systemProbe: LaunchdProbeResult = .notRegistered
}

extension DaemonOwnership {
    /// The canonical launchd label, shared by the app's own per-user agent and the
    /// installed system daemon (they differ only by launchd DOMAIN — `gui/` vs
    /// `system/`). Disambiguated by self-status, never by the label: a system or
    /// foreign `launchctl bootstrap` never flips the app's SMAppService status to
    /// `.enabled`.
    static let canonicalLabel = "com.coderinserepeat.runnyd"

    /// Reduce the three gathered facts to a verdict. The ordering is load-bearing:
    ///
    ///   1. a non-canonical home defers FIRST (a can't-happen override guard);
    ///   2. a registered system daemon wins next — it owns the shared socket the app
    ///      dials, ahead of our own per-user agent (system + our agent is a real
    ///      two-manager conflict the install gate must keep out);
    ///   3. a wedged system probe then fails CLOSED — a system daemon MIGHT be here,
    ///      so never install over (or manage our own agent ahead of) what we can't
    ///      rule out;
    ///   4. with no system daemon, the verdict is the app's own per-user agent's life
    ///      stage, read from the authoritative SMAppService self-status.
    nonisolated static func classify(_ inputs: DaemonOwnershipInputs) -> DaemonOwnership {
        if !inputs.homeIsCanonical { return .indeterminate }
        if inputs.systemProbe == .registered { return .systemManaged }
        if inputs.systemProbe == .indeterminate { return .indeterminate }
        switch inputs.selfState {
        case .installed:
            return .selfManaged
        case .requiresApproval:
            return .awaitingApproval
        case .registrationFailed:
            // An unrecognized future SMAppService status is a determination FAILURE, not
            // a confirmed not-installed — fail closed so an unknown registration state
            // never becomes install permission.
            return .indeterminate
        case .notInstalled, .notFound:
            return .unmanaged
        }
    }
}
