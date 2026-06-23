import Foundation

/// Who manages the daemon, as a pure verdict over the facts the app gathers:
/// SMAppService self-status, two launchd-label probes, the socket axis, a
/// home-canonical flag, and whether a dormant manual plist persists on disk. No
/// side effects — `classify` is a `nonisolated static`
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
    /// A runnyd is registered in the `system/` launchd domain — the installed
    /// non-root system daemon. The app installs and removes it via the system path
    /// (`runnyctl install-daemon` / `uninstall-daemon`, brokered through the app or
    /// run directly), observes it over the shared socket, and never installs a
    /// competing per-user agent over it.
    case systemManaged
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
    /// Whether the canonical label is registered in the `system/` domain — a
    /// non-root system daemon (the headless deployment). Defaults `.notRegistered`
    /// so a host with no system daemon — the common case, and every existing
    /// caller/test — behaves exactly as before.
    var systemProbe: LaunchdProbeResult = .notRegistered
    /// Whether a daemon answers the socket.
    var socketAnswers: Bool
    /// Whether the manual installer's plist persists at
    /// `~/Library/LaunchAgents/com.coderinserepeat.runnyd.plist`. The launchd probes
    /// see only what is *loaded*; a manual agent that was `bootout`'d but not `rm`'d
    /// leaves its plist on disk, which launchd auto-loads at next login. The app never
    /// writes there (SMAppService uses the in-bundle plist), so a file at that path is
    /// unambiguously a foreign manual install — a dormant owner the probes are blind to.
    var manualPlistPersisted: Bool
}

/// Every competing daemon registration the host carries, beyond the single owner
/// `DaemonOwnership.classify`'s verdict names. The spawn gate needs only the verdict
/// (one allow/deny); the UI needs the full set so remediation clears ALL contenders,
/// not just the first the precedence surfaces. A host can carry more than one — brew
/// + a manual plist, our own agent + a dormant manual plist — the latent split-brain
/// a single verdict can't express. Pure over the same inputs as `classify`.
struct DaemonOwnershipCollisions: Equatable {
    /// Homebrew's `homebrew.mxcl.runny` is registered.
    var brew = false
    /// Our own `SMAppService` agent is registered (enabled OR awaiting approval) — a
    /// competitor the app can withdraw in-process via `unregister()`, with no bootout
    /// of the shared canonical label.
    var ownAgent = false
    /// The canonical label is loaded by a NON-self installer (a foreign manual job
    /// running now — bootout is needed to stop it, and is safe because it isn't ours).
    var manualLoaded = false
    /// A dormant manual plist persists at `~/Library/LaunchAgents` — launchd reloads
    /// it at next login. `rm` clears it; a bootout must NOT be issued for this alone,
    /// since when our own agent is enabled it holds the very label a bootout targets.
    var manualPlist = false

    /// A foreign manual registration is present in either form (loaded or dormant).
    var manual: Bool { manualLoaded || manualPlist }
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
        // 2. A system daemon owns the SHARED socket the app dials first (clients resolve
        //    shared-then-per-user), so it surfaces ahead of every per-user owner —
        //    INCLUDING brew — so the verdict (and its banner) names the daemon the app
        //    actually reaches, not a co-registered (leftover-migration) brew label it
        //    doesn't. Also ahead of self (system + our own agent is a real two-manager
        //    conflict) and the foreground branch (a system daemon answering the shared
        //    socket must be named, not mislabeled a hand-run daemon).
        if inputs.systemProbe == .registered { return .systemManaged }
        // 3. A registered Homebrew service is a foreign daemon on its OWN label, so a
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
            // Approving launches the RunAtLoad agent OUTSIDE the spawn gate (a System
            // Settings action), so awaitingApproval is safe only once a foreign owner is
            // DEFINITIVELY ruled out: both probes confirmed `.notRegistered`, no socket,
            // AND no dormant manual plist on disk. A registered label, an occupied socket,
            // an inconclusive probe (a stopped-but-registered brew service that timed out,
            // say), OR a persisted manual plist (which launchd reloads at next login)
            // means a competing owner might be present — defer.
            if inputs.brewProbe == .notRegistered, inputs.canonicalProbe == .notRegistered,
               inputs.systemProbe == .notRegistered,
               !inputs.socketAnswers, !inputs.manualPlistPersisted
            {
                return .awaitingApproval
            }
            return .indeterminate
        case .registrationFailed:
            // An unrecognized future SMAppService status is a determination FAILURE, not
            // a confirmed not-installed — fail closed so an unknown registration state
            // never becomes install permission.
            return .indeterminate
        case .notInstalled, .notFound:
            break
        }
        // 5. A manual installer owns it — either its canonical label is loaded now, OR
        //    its plist persists on disk (dormant after a `bootout` without `rm`, which
        //    launchd reloads at next login). Both are foreign manual installs the app
        //    must not displace; the on-disk plist is the signal the loaded-label probe
        //    is blind to, and the one that turns this from `.unmanaged` into a stomp.
        if inputs.canonicalProbe == .registered || inputs.manualPlistPersisted { return .foreignManual }
        // 6. An inconclusive probe dominates the PERMISSIVE verdicts below: "not sure
        //    who owns this" must never read as install-a-second-manager (unmanaged) or
        //    stop-a-hand-run-daemon (foreground). Determinate foreign owners already
        //    surfaced above; both they and indeterminate deny, so naming the known
        //    owner is strictly better than deferring.
        if inputs.brewProbe == .indeterminate || inputs.canonicalProbe == .indeterminate
            || inputs.systemProbe == .indeterminate
        {
            return .indeterminate
        }
        // 7. A daemon answers but no agent is registered — a hand-run runnyd.
        if inputs.socketAnswers { return .foreground }
        // 8. Nothing owns it — install allowed.
        return .unmanaged
    }

    /// Every competing registration the host carries, independent of which one
    /// `classify`'s verdict names. The verdict drives the gate (one owner); this
    /// drives the UI's cleanup affordances, which must reach EVERY contender. The
    /// shared canonical label is disambiguated by self-status exactly as `classify`
    /// does: a registered canonical label is OURS when self is `.installed`, so it
    /// counts as a foreign *manual* load only when self is not `.installed` (a pending
    /// agent isn't running and so can't be holding the loaded label). This `!= .installed`
    /// rule MUST move in lockstep with `classify`'s self-identity ordering above; if that
    /// ever changes which self-state holds the live label, change `manualLoaded` with it.
    /// The safety direction is structural regardless: under a `selfManaged` verdict self
    /// is necessarily `.installed`, so `manualLoaded` is false and `manualCleanupCommand`
    /// can never bootout the label our own agent holds.
    nonisolated static func collisions(_ inputs: DaemonOwnershipInputs) -> DaemonOwnershipCollisions {
        DaemonOwnershipCollisions(
            brew: inputs.brewProbe == .registered,
            ownAgent: inputs.selfState == .installed || inputs.selfState == .requiresApproval,
            manualLoaded: inputs.canonicalProbe == .registered && inputs.selfState != .installed,
            manualPlist: inputs.manualPlistPersisted
        )
    }
}
