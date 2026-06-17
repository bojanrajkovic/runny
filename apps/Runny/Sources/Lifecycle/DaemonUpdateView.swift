import SwiftUI

/// The post-upgrade daemon-update affordance for the menu bar and main window.
/// Shown only when the app installed the agent AND is the newer build: Update
/// issues a drain-gated reload (jobs finish first, then launchd cold-starts the
/// freshly-bundled binary). A non-converged result is named loud — "still vX" —
/// not folded into the generic reload note. Self-hides otherwise.
struct DaemonUpdateAffordance: View {
    @Environment(DaemonStore.self) private var store
    @Environment(AgentController.self) private var agent

    var body: some View {
        switch store.daemonUpdate(agentInstalled: agent.installState == .installed) {
        case .none:
            EmptyView()
        case .available:
            row(
                "A newer runnyd ships with this app. Update drains running jobs first, then restarts.",
                tint: .orange
            ) {
                Button("Update Daemon") { store.requestDaemonUpdate() }
            }
        case .inProgress:
            row("Updating runnyd — draining running jobs first…", tint: .secondary) {
                ProgressView().controlSize(.small)
            }
        case let .didNotTake(core):
            row("Update didn't take — runnyd is still \(core).", tint: .red) {
                Button("Try Again") { store.requestDaemonUpdate() }
            }
        }
    }

    private func row(_ text: String, tint: Color, @ViewBuilder action: () -> some View) -> some View {
        HStack(spacing: 6) {
            Image(systemName: "arrow.down.circle")
                .font(.caption)
                .foregroundStyle(tint)
            Text(text)
                .font(.caption)
                .foregroundStyle(.primary)
                .fixedSize(horizontal: false, vertical: true)
            Spacer(minLength: 4)
            action()
        }
        .padding(.horizontal, Metrics.pad)
        .padding(.vertical, 6)
        .background(tint == .secondary ? Color.clear : tint.opacity(0.08))
    }
}
