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
        VStack(alignment: .leading, spacing: 6) {
            updateAffordance
            gateRows
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

    @ViewBuilder private var updateAffordance: some View {
        switch store.daemonUpdate(
            // Require BOTH ownership and installState — each guards a distinct way the
            // other goes stale. ownership == .selfManaged rejects a verdict the app
            // doesn't drain-update: a systemManaged daemon (managed from Settings →
            // System Service, not a per-user drain-respawn) or any deferring verdict.
            // installState == .installed rejects a stale .selfManaged left behind by a
            // teardown the verdict didn't re-gather (a partial uninstall/failed repair
            // where the agent is gone but the daemon lingers connected) — Updating that
            // would drain a daemon with no agent to respawn it. The AND can't be fooled
            // by a single stale signal.
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
                // Gate on the bundled (new) runnyd validating the in-place config
                // before any reload: OK proceeds, Warn surfaces warnings + confirms,
                // Error blocks loud. requestDaemonUpdate is reached only via the gate.
                await store.gatedDaemonUpdate(
                    runnydPath: SystemDaemonInstaller.bundleRunnydPath,
                    configPath: RunnyHome.directory.appendingPathComponent("config.yaml").path
                )
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
