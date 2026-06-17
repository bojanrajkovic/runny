import ServiceManagement
import SwiftUI

/// The Settings "Daemon" row: a start-at-login toggle that registers/unregisters
/// the LaunchAgent, plus a loud status line for every non-installed state. State
/// is read from `AgentController` (derived from `SMAppService.status`), never an
/// action's return. The toggle is gated on install eligibility (translocation /
/// not-in-`/Applications`), and turning it ON first raises a confirmation naming
/// the launchd label — so a brew-managed daemon is never silently displaced
/// before detect-and-defer lands.
struct AgentInstallRow: View {
    @Environment(AgentController.self) private var agent
    @Environment(DaemonStore.self) private var store
    @State private var confirmingInstall = false
    @State private var confirmingUninstall = false

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            Toggle("Start runnyd at login", isOn: toggleBinding)
                .disabled(!canToggle)
            if let detail {
                Text(detail)
                    .font(.caption)
                    .foregroundStyle(tint)
                    .textSelection(.enabled)
                    .fixedSize(horizontal: false, vertical: true)
            }
            if agent.installState == .requiresApproval {
                Button("Open Login Items…") { SMAppService.openSystemSettingsLoginItems() }
                    .controlSize(.small)
            }
            if let reconcile = reconcileWarning {
                Label(reconcile, systemImage: "exclamationmark.triangle")
                    .font(.caption)
                    .foregroundStyle(.orange)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
        .padding(.vertical, 2)
        .confirmationDialog(
            "Install the runnyd login agent?",
            isPresented: $confirmingInstall,
            titleVisibility: .visible
        ) {
            Button("Install") { Task { await agent.install() } }
            Button("Cancel", role: .cancel) {}
        } message: {
            Text("Runny will register the launchd agent “\(SMAppServiceRegistrar.agentLabel)” to run "
                + "runnyd in your login session and start it at login. If another tool already manages "
                + "runnyd, cancel and remove that first.")
        }
        .confirmationDialog(
            "Remove runnyd while a job is running?",
            isPresented: $confirmingUninstall,
            titleVisibility: .visible
        ) {
            Button("Remove and Abandon Job", role: .destructive) { Task { await agent.uninstall() } }
            Button("Cancel", role: .cancel) {}
        } message: {
            Text("Removing the daemon stops it immediately, abandoning the running "
                + "job on \(abandonedSlotsText). The job will not finish.")
        }
    }

    /// ON when the agent is registered (installed, or registered-but-awaiting Login
    /// Items approval). Turning ON raises the install confirmation; turning OFF
    /// unregisters — but mid-job it raises the abandon-the-job confirmation first
    /// (uninstall boots out the daemon, killing the in-process VM). The toggle
    /// reflects the true state until the action confirms, so a cancelled dialog
    /// leaves it where it was.
    private var toggleBinding: Binding<Bool> {
        Binding(
            get: { agent.installState == .installed || agent.installState == .requiresApproval },
            set: { wantOn in
                if wantOn {
                    confirmingInstall = true
                } else if store.runningJobSlots.isEmpty {
                    Task { await agent.uninstall() }
                } else {
                    confirmingUninstall = true
                }
            }
        )
    }

    private var abandonedSlotsText: String {
        let slots = store.runningJobSlots
        return slots.count == 1 ? "slot \(slots[0])" : "slots \(slots.joined(separator: ", "))"
    }

    private var reconcileWarning: String? {
        switch agent.reconcileState {
        case .ok: nil
        case let .foreign(path): "A runnyd agent is registered from an unexpected location (\(path)). "
            + "Reinstall from /Applications to repoint it."
        case .undetermined: "Couldn't determine the registered runnyd agent's location."
        }
    }

    /// The toggle is actionable only from an eligible location and when there is a
    /// bundled daemon to register. `notFound` is a dev build with no bundled plist;
    /// `registrationFailed` stays toggle-able so the operator can retry.
    private var canToggle: Bool {
        guard agent.eligibility == .eligible else { return false }
        return agent.installState != .notFound
    }

    private var detail: String? {
        switch agent.eligibility {
        case .translocated:
            return "Runny is running from a temporary location. Quit it and re-open from your "
                + "Applications folder, then try again."
        case let .notInApplications(path):
            return "Move Runny to your Applications folder to install the daemon (currently at \(path))."
        case .eligible:
            break
        }
        switch agent.installState {
        case .notInstalled:
            return "runnyd does not start automatically. Turn on to install it as a login agent."
        case .installed:
            return "runnyd starts at login, and a drain-gated reload applies upgrades."
        case .requiresApproval:
            return "Approval is pending in Login Items before runnyd can start."
        case .notFound:
            return "This build carries no bundled daemon to install."
        case let .registrationFailed(reason):
            return reason
        }
    }

    private var tint: Color {
        if case .registrationFailed = agent.installState { return .red }
        if agent.eligibility != .eligible { return .orange }
        switch agent.installState {
        case .requiresApproval, .notFound: return .orange
        case .notInstalled, .installed, .registrationFailed: return .secondary
        }
    }
}
