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
        let verdict = daemonUpdateVerdict(store, agent)
        VStack(alignment: .leading, spacing: 6) {
            updateAffordance(verdict)
            // Gate rows belong to an available/in-flight update — render them only
            // while one is on offer, so a Warn/block doesn't linger after the update
            // converges to .none (the state is also cleared on convergence/reconnect).
            if verdict != .none {
                gateRows
            }
        }
    }

    /// The config-compat gate's surfaced state: a "checking…" row while the probe
    /// runs, a loud red block when the new runnyd refuses the config, and the
    /// warnings to confirm past on a Warn verdict. Plain conditionals (no
    /// transitions) per the hosted-SwiftUI rule.
    @ViewBuilder private var gateRows: some View {
        if store.configGateRunning {
            AffordanceRow(systemImage: icon, text: "Checking the new runnyd accepts the current config…", tint: .secondary) {
                ProgressView().controlSize(.small)
            }
        }
        if let block = store.configGateBlock {
            AffordanceRow(systemImage: "exclamationmark.triangle.fill", text: "Update blocked — \(block)", tint: .red) {
                EmptyView()
            }
        }
        ForEach(store.configGateWarnings, id: \.message) { warning in
            AffordanceRow(systemImage: "exclamationmark.circle", text: "Config warning — \(warning.message)", tint: .orange) {
                EmptyView()
            }
        }
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

    /// The update's confirmation dialog is hosted only on the main window, so —
    /// like the footer Reload button — open the main window first. From the main
    /// window itself this just refocuses it; from the popover it's load-bearing
    /// (otherwise the dialog has no presenter and the update silently no-ops).
    private func update() {
        activation.openMainWindow(openWindow)
        Task {
            // Re-gather before arming the drain-gated update: Update fires outside the
            // spawn gate, so a foreign daemon that took over while the window stayed open
            // must cancel it — draining a foreign fleet for an update that can't take is
            // active harm, not a no-op. The render gate can be minutes stale by click.
            if await agent.revalidate(.selfManaged), agent.installState == .installed {
                // Gate on the bundled (new) runnyd validating the in-place config
                // before any reload: OK proceeds, Warn surfaces warnings + confirms,
                // Error blocks loud. requestDaemonUpdate is reached only via the gate,
                // and the gate is re-run at the confirmed reload (performReload).
                await store.gatedDaemonUpdate()
            }
        }
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
