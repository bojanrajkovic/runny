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
        // A stable Group host so the recovery .onChange fires even when the
        // affordance self-hides (the row disappears the instant the daemon is
        // reachable, which is exactly when we want to clear a terminal Start outcome).
        Group {
            switch LaunchAgentStatus.startAffordance(
                state: agent.installState, daemonUnreachable: daemonUnreachable, canonical: agentCanonical
            ) {
            case .none:
                EmptyView()
            case .approval:
                AffordanceRow(
                    systemImage: icon,
                    text: "runnyd is installed but needs approval in Login Items.",
                    tint: .orange
                ) {
                    Button("Approve…") { SMAppService.openSystemSettingsLoginItems() }
                }
            case .start:
                startRow
            }
        }
        .onChange(of: daemonUnreachable) { _, unreachable in
            if !unreachable { agent.noteRecovered() }
        }
    }

    @ViewBuilder
    private var startRow: some View {
        switch agent.startOutcome {
        case .starting:
            AffordanceRow(systemImage: icon, text: "Starting runnyd…", tint: .secondary) {
                ProgressView().controlSize(.small)
            }
        case .didNotComeUp:
            AffordanceRow(systemImage: icon, text: "Start issued, but runnyd hasn't come up.", tint: .orange) {
                Button("Try Again") { startDaemon() }
            }
        case let .refused(reason):
            AffordanceRow(systemImage: icon, text: reason, tint: .red) {
                Button("Try Again") { startDaemon() }
            }
        case .idle, .cameUp:
            AffordanceRow(systemImage: icon, text: "runnyd is installed but not running.", tint: .secondary) {
                Button("Start") { startDaemon() }
            }
        }
    }

    private let icon = "bolt.horizontal.circle"

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

    /// Start requires affirmative canonical confirmation (`.ok`), not the unchecked
    /// default — kickstarting a foreign agent would start the wrong binary. The
    /// surfaces run reconcile on appear, so this resolves shortly after.
    private var agentCanonical: Bool {
        agent.reconcileState == .ok
    }
}
