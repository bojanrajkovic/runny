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
    @State private var confirmingInstall = false

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
    }

    /// ON when the agent is registered (installed, or registered-but-awaiting Login
    /// Items approval). Turning ON raises the confirmation rather than registering
    /// immediately; the toggle reflects the true state until install confirms, so a
    /// cancelled confirmation leaves it OFF. Turning OFF unregisters.
    private var toggleBinding: Binding<Bool> {
        Binding(
            get: { agent.installState == .installed || agent.installState == .requiresApproval },
            set: { wantOn in
                if wantOn { confirmingInstall = true } else { Task { await agent.uninstall() } }
            }
        )
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
