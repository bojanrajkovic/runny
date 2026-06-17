import ServiceManagement
import SwiftUI

/// The "daemon not running" affordance for the menu bar and main window. Renders:
/// a Start button when the agent is installed but the daemon is unreachable; the
/// Login Items approval CTA when approval is pending (NEVER a dead Start that would
/// kickstart a job launchd refuses); and a loud "Start issued but runnyd hasn't
/// come up" when a kickstart doesn't take within the bound. Self-hides otherwise,
/// so callers can place it unconditionally. Start confirms recovery from the
/// connection, never the kickstart return.
struct DaemonStartAffordance: View {
    @Environment(AgentController.self) private var agent
    @Environment(DaemonStore.self) private var store

    var body: some View {
        switch LaunchAgentStatus.startAffordance(state: agent.installState, daemonUnreachable: daemonUnreachable) {
        case .none:
            EmptyView()
        case .approval:
            row(
                text: "runnyd is installed but needs approval in Login Items.",
                tint: .orange
            ) {
                Button("Approve…") { SMAppService.openSystemSettingsLoginItems() }
            }
        case .start:
            startRow
        }
    }

    @ViewBuilder
    private var startRow: some View {
        switch agent.startOutcome {
        case .starting:
            row(text: "Starting runnyd…", tint: .secondary) {
                ProgressView().controlSize(.small)
            }
        case .didNotComeUp:
            row(text: "Start issued, but runnyd hasn't come up.", tint: .orange) {
                Button("Try Again") { startDaemon() }
            }
        case let .refused(reason):
            row(text: reason, tint: .red) {
                Button("Try Again") { startDaemon() }
            }
        case .idle, .cameUp:
            row(text: "runnyd is installed but not running.", tint: .secondary) {
                Button("Start") { startDaemon() }
            }
        }
    }

    private func startDaemon() {
        Task {
            await agent.start(isConnected: {
                if case .connected = store.connection { true } else { false }
            })
        }
    }

    private var daemonUnreachable: Bool {
        if case .unreachable = store.connection { true } else { false }
    }

    private func row(text: String, tint: Color, @ViewBuilder action: () -> some View) -> some View {
        HStack(spacing: 6) {
            Image(systemName: "bolt.horizontal.circle")
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
