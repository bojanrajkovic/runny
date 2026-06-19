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
        switch store.daemonUpdate(
            // Require BOTH ownership and installState — each guards a distinct way the
            // other goes stale. ownership == .selfManaged rejects a Homebrew collision
            // (our agent .installed but brew's, possibly older, daemon is what runs, so a
            // drain-gated Update can't take). installState == .installed rejects a stale
            // .selfManaged left behind by a teardown the verdict didn't re-gather (a
            // partial uninstall/failed repair where the agent is gone but the daemon
            // lingers connected) — Updating that would drain a daemon with no agent to
            // respawn it. The AND can't be fooled by a single stale signal.
            agentInstalled: agent.ownership == .selfManaged && agent.installState == .installed,
            agentCanonical: agentCanonical,
            runningBundleCanonical: agent.eligibility == .eligible
        ) {
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
                store.requestDaemonUpdate()
            }
        }
    }

    /// Update requires AFFIRMATIVE canonical confirmation — `.ok`, not the
    /// unchecked `.notChecked` default (nor `.foreign`/`.undetermined`). A reload
    /// for a foreign or unverified agent could respawn the wrong BundleProgram, so
    /// the surfaces run reconcile on appear and Update stays hidden until it lands.
    private var agentCanonical: Bool {
        agent.reconcileState == .ok
    }
}
