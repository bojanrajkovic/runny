import Foundation

/// Who manages the daemon, as a pure verdict over the facts the app gathers:
/// SMAppService self-status, two launchd-label probes, the socket axis, and a
/// home-canonical flag. No side effects — `classify` is a `nonisolated static`
/// function over value types, unit-tested without a live daemon or launchd.
/// Mirrors `LaunchAgentStatus`'s pure-verdict shape; the impure probe that
/// produces `LaunchdProbeResult` lives in `LaunchdProbe`.
enum DaemonOwnership: Equatable {
    /// No manager owns the daemon — install is allowed.
    case unmanaged
    /// The app's own SMAppService agent owns it (self-status `.enabled`).
    case selfManaged
    /// Homebrew's `homebrew.mxcl.runny` agent owns it.
    case foreignBrew
    /// The canonical `com.coderinserepeat.runnyd` label is registered, but not by
    /// us — a manual `launchctl` installer.
    case foreignManual
    /// A daemon answers the socket but no agent is registered — a hand-run runnyd.
    case foreground
    /// Our own agent is registered but awaiting Login Items approval.
    case awaitingApproval
    /// A probe was inconclusive, or the home is non-canonical: defer with a
    /// diagnostic — never install over, never kill.
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

/// The orthogonal facts `classify` reduces to a verdict, gathered by the app
/// (`AgentController.refreshOwnership`). Pure here.
struct DaemonOwnershipInputs: Equatable {
    /// Whether the resolved runny home is the canonical `~/.runny`. Always true
    /// now that the home is fixed (the override is gone); kept as a defense-in-depth
    /// guard so a re-introduced override can never cause a cross-home stomp.
    var homeIsCanonical: Bool
    /// The app's own agent status, already mapped (`.installed` == SMAppService
    /// `.enabled` == ours; the C1 spike confirmed a foreign owner never reads
    /// `.enabled`, so this is an authoritative self-identity signal).
    var selfState: LaunchAgentStatus.State
    /// Whether `homebrew.mxcl.runny` is registered.
    var brewProbe: LaunchdProbeResult
    /// Whether `com.coderinserepeat.runnyd` is registered (ours OR a manual one).
    var canonicalProbe: LaunchdProbeResult
    /// Whether a daemon answers the socket.
    var socketAnswers: Bool
}

extension DaemonOwnership {
    /// The launchd labels runny cares about. The brew label is synthesized by
    /// Homebrew as `homebrew.mxcl.<formula>`; verified against a real `brew
    /// services` install. The canonical label is shared by the app's own agent and
    /// any manual installer — disambiguated by self-status, never by the label.
    static let brewLabel = "homebrew.mxcl.runny"
    static let canonicalLabel = "com.coderinserepeat.runnyd"

    /// Reduce the gathered facts to a verdict. The ordering is load-bearing, and
    /// the key distinction is *inconclusive* vs *affirmative*: authoritative
    /// self-identity overrides an inconclusive probe (so a wedge can't block managing
    /// our own daemon) but NOT an affirmative foreign registration (so a second
    /// manager is never hidden). A registered Homebrew service is foreign on its own
    /// label, so it surfaces even ahead of self. An inconclusive probe then dominates
    /// only the PERMISSIVE verdicts below it (foreground, unmanaged) — never the
    /// determinate foreign owners, which are strictly more informative and also deny.
    nonisolated static func classify(_ inputs: DaemonOwnershipInputs) -> DaemonOwnership {
        // 1. Non-canonical home: the socket and label axes would describe different
        //    homes. Defense-in-depth — the home is fixed now, but a re-introduced
        //    override must never let the app install over a daemon at the real home.
        if !inputs.homeIsCanonical { return .indeterminate }
        // 2. A registered Homebrew service is a foreign daemon on its OWN label, so a
        //    positive brew probe surfaces even when our own agent is also enabled —
        //    that is a real two-manager conflict, not a self-managed host.
        if inputs.brewProbe == .registered { return .foreignBrew }
        // 3-4. Authoritative self-identity. `.enabled` (`.installed`) means the
        //      canonical label is ours (the C1 spike: a foreign owner never reads
        //      `.enabled`), so a wedged probe can't flip us to `indeterminate` and
        //      block managing/starting our own daemon.
        switch inputs.selfState {
        case .installed:
            return .selfManaged
        case .requiresApproval:
            // Our registration is pending approval (not necessarily bootstrapped), so a
            // bootstrapped canonical label is ambiguous — ours-pending vs a foreign
            // manual one. Defer rather than expose app-owned teardown, since uninstall
            // bootouts the shared label and could stop a foreign daemon.
            return inputs.canonicalProbe == .registered ? .indeterminate : .awaitingApproval
        case .registrationFailed:
            // An unrecognized future SMAppService status is a determination FAILURE, not
            // a confirmed not-installed — fail closed so an unknown registration state
            // never becomes install permission.
            return .indeterminate
        case .notInstalled, .notFound:
            break
        }
        // 5. The canonical label is registered but not ours — a manual installer.
        if inputs.canonicalProbe == .registered { return .foreignManual }
        // 6. An inconclusive probe dominates the PERMISSIVE verdicts below: "not sure
        //    who owns this" must never read as install-a-second-manager (unmanaged) or
        //    stop-a-hand-run-daemon (foreground). Determinate foreign owners already
        //    surfaced above; both they and indeterminate deny, so naming the known
        //    owner is strictly better than deferring.
        if inputs.brewProbe == .indeterminate || inputs.canonicalProbe == .indeterminate {
            return .indeterminate
        }
        // 7. A daemon answers but no agent is registered — a hand-run runnyd.
        if inputs.socketAnswers { return .foreground }
        // 8. Nothing owns it — install allowed.
        return .unmanaged
    }
}
