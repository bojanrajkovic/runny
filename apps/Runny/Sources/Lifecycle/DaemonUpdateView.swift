import SwiftUI

/// The post-upgrade daemon-update affordance for the menu bar and main window.
/// Shown only when the app installed the agent AND is the newer build: Update
/// issues a drain-gated reload (jobs finish first, then launchd cold-starts the
/// freshly-bundled binary). A non-converged result is named loud — "still vX" —
/// not folded into the generic reload note. Self-hides otherwise.
struct DaemonUpdateAffordance: View {
    @Environment(DaemonStore.self) private var store
    @Environment(AgentController.self) private var agent
    @Environment(ActivationCoordinator.self) private var activation
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        // The gate verdict is surfaced as popups (ConfigGateAlerts), not inline rows —
        // a row rendered behind the modal reload prompt was easy to miss.
        updateAffordance(daemonUpdateVerdict(store, agent))
    }

    @ViewBuilder private func updateAffordance(_ verdict: DaemonStore.DaemonUpdate) -> some View {
        switch verdict {
        case .none:
            EmptyView()
        case .available:
            AffordanceRow(
                systemImage: icon,
                text: "A newer runnyd ships with this app. Update drains running jobs first, then restarts.",
                tint: .orange
            ) {
                Button("Update Daemon") { update() }
            }
        case .inProgress:
            AffordanceRow(systemImage: icon, text: "Updating runnyd — draining running jobs first…", tint: .secondary) {
                ProgressView().controlSize(.small)
            }
        case let .didNotTake(core):
            AffordanceRow(systemImage: icon, text: "Update didn't take — runnyd is still \(core).", tint: .red) {
                Button("Try Again") { update() }
            }
        }
    }

    private let icon = "arrow.down.circle"

    /// The gate's popups are hosted on the main window (the popover panel has no
    /// reliable presenter), so open it first — a no-op refocus from the window itself,
    /// load-bearing from the popover.
    private func update() {
        activation.openMainWindow(openWindow)
        Task { await startGatedReload(store, agent, explicitUpdate: true) }
    }
}

/// The daemon-update verdict for the current store + agent — shared by the update
/// affordance and by the plain Reload buttons (which must gate a reload that would
/// respawn the newer bundled binary). It requires BOTH ownership and installState,
/// each guarding a distinct staleness: `ownership == .selfManaged` rejects a verdict
/// the app doesn't drain-update (a `systemManaged` daemon, or any deferring verdict);
/// `installState == .installed` rejects a stale `.selfManaged` left by a teardown the
/// verdict didn't re-gather. The canonical checks (`reconcileState == .ok`,
/// `eligibility == .eligible`) require AFFIRMATIVE confirmation that the registered
/// agent and the running bundle are THIS `/Applications` app, so the verdict reflects
/// the binary a reload would actually respawn — never a translocated or foreign one.
/// `!= .none` therefore means "a reload right now would cold-start a newer bundled
/// daemon", the precise condition under which a plain reload is really an update.
@MainActor
func daemonUpdateVerdict(_ store: DaemonStore, _ agent: AgentController) -> DaemonStore.DaemonUpdate {
    store.daemonUpdate(
        agentInstalled: agent.ownership == .selfManaged && agent.installState == .installed,
        agentCanonical: agent.reconcileState == .ok,
        runningBundleCanonical: agent.eligibility == .eligible
    )
}

/// Whether a reload would respawn our NEWER bundle, and so must run the config-compat
/// gate. **Two facts decide it, and only two:**
///
/// - **Ahead?** The app must be a newer build than the running daemon — a live
///   version/protocol compare (`appAheadOfDaemon`). Not ahead → can't upgrade.
/// - **Ours?** The reload must respawn OUR bundle, which only the *owner* determines:
///   a `selfManaged` daemon is our per-user agent, so a reload cold-starts our
///   `BundleProgram` — gate it. `indeterminate` (a wedged system probe) can't be told
///   apart from ours, so fail closed. A settled non-self owner respawns its OWN binary,
///   validated by its own reload preflight — not ours to gate.
///
/// Reconcile/canonical-ness and the affordance verdict deliberately do NOT enter: a
/// `selfManaged` daemon respawns our `BundleProgram` regardless of them. Collapsing to
/// these two axes is what stops this from sprouting an edge case per ownership ×
/// reconcile × version cell.
@MainActor
func reloadMightUpgrade(_ store: DaemonStore, _ agent: AgentController) -> Bool {
    guard store.appAheadOfDaemon else { return false }
    switch agent.ownership {
    // ponytail: `.indeterminate` fails closed though it's usually a foreign/system
    // daemon — accepting a rare, transient, retryable false-block, because the
    // alternative is a crash-loop if it really is our per-user agent, and
    // crash-loop-proof beats false-block.
    case .selfManaged, .indeterminate:
        return true
    case .unmanaged, .systemManaged, .awaitingApproval:
        return false
    }
}

/// The single entry point both reload affordances call, distinguished only by
/// `explicitUpdate` — true for the **Update Daemon** affordance (the click is the
/// drain consent), false for the plain **Reload Config** button (its dialog is).
///
/// Re-gather ownership first — the button can be stale, and a reload drains the fleet,
/// so a daemon that changed hands must not be drained for an upgrade it can't take.
/// Then: an upgrade reload runs the config-compat gate (shared popups; OK differs by
/// `explicitUpdate`). A reload that can't upgrade is a plain config reload for the
/// Reload Config button — but for an **Update** click whose ownership/version no longer
/// warrants it (slipped to a foreign/system daemon, or already current), REFUSE rather
/// than fall through to draining a daemon we don't own; the affordance re-renders and
/// self-hides on the re-gathered facts.
@MainActor
func startGatedReload(_ store: DaemonStore, _ agent: AgentController, explicitUpdate: Bool) async {
    _ = await agent.revalidate(.selfManaged)
    if reloadMightUpgrade(store, agent) {
        await store.gatedDaemonUpdate(explicitUpdate: explicitUpdate)
    } else if !explicitUpdate {
        store.requestReload()
    }
}

/// The config-compat gate's popups, hosted on the main window root alongside the
/// command alerts and the generic reload confirm. The gate verdict IS the prompt:
/// a Warn presents a confirm-or-cancel alert (Cancel is the safe default; "Reload
/// Anyway" is the destructive, deliberate action); an Error presents an
/// acknowledge-only alert that reloads nothing. OK shows no popup — it reloads
/// straight away, since clicking Update/Reload was already the consent.
struct ConfigGateAlerts: ViewModifier {
    @Environment(DaemonStore.self) private var store

    func body(content: Content) -> some View {
        content
            .alert("Config has warnings", isPresented: warnPresented) {
                Button("Reload Anyway", role: .destructive) { store.confirmGatedUpdate() }
                Button("Cancel", role: .cancel) { store.clearConfigGate() }
            } message: {
                Text(warnMessage)
            }
            .alert("Can’t update runnyd", isPresented: blockPresented) {
                Button("OK", role: .cancel) { store.clearConfigGate() }
            } message: {
                Text("The newer runnyd rejects the current config, so nothing was changed:\n\n\(store.configGateBlock ?? "")")
            }
    }

    private var warnPresented: Binding<Bool> {
        Binding(get: { !store.configGateWarnings.isEmpty }, set: { if !$0 { store.clearConfigGate() } })
    }

    private var blockPresented: Binding<Bool> {
        Binding(get: { store.configGateBlock != nil }, set: { if !$0 { store.clearConfigGate() } })
    }

    private var warnMessage: String {
        let lines = store.configGateWarnings.map { "• \($0.message)" }.joined(separator: "\n")
        return "The newer runnyd accepts this config but flagged:\n\n\(lines)\n\nReload onto it anyway?"
    }
}

extension View {
    func configGateAlerts() -> some View { modifier(ConfigGateAlerts()) }
}
