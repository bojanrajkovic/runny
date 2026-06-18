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
    @State private var confirmingRepair = false

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            ownershipAwareContent
        }
        .padding(.vertical, 2)
        .task { await agent.refreshOwnership() }
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
            "Remove runnyd while a guest is live?",
            isPresented: $confirmingUninstall,
            titleVisibility: .visible
        ) {
            Button("Remove and Abandon", role: .destructive) { Task { await agent.uninstall() } }
            Button("Cancel", role: .cancel) {}
        } message: {
            if store.liveGuestSlots.isEmpty {
                // Reached only when the live-guest state is UNKNOWN (the daemon isn't
                // connected), so we can't prove nothing is running — warn fail-safe.
                Text("Runny can't confirm whether a job is running because the daemon "
                    + "isn't connected. Removing it now may abandon a running job or a "
                    + "debug-held guest.")
            } else {
                Text("Removing the daemon stops it immediately, abandoning the work on "
                    + "\(abandonedSlotsText). A running job will not finish, and a "
                    + "debug-held guest is destroyed.")
            }
        }
        .confirmationDialog(
            "Repair the runnyd login agent?",
            isPresented: $confirmingRepair,
            titleVisibility: .visible
        ) {
            Button("Repair") { Task { await agent.repair() } }
            Button("Cancel", role: .cancel) {}
        } message: {
            // The same foreign-manager guard as install: re-registering displaces
            // whatever holds the label, and until detect-and-defer lands the spawn
            // gate is .allow, so this consent is the guard against stomping a
            // brew-managed daemon registered under the same label.
            Text("A runnyd agent is registered from an unexpected location. Repair re-registers "
                + "Runny's bundled daemon under the launchd agent “\(SMAppServiceRegistrar.agentLabel)”, "
                + "replacing it. If another tool (e.g. Homebrew) manages runnyd, cancel and remove that first.")
        }
    }

    /// The Daemon row content, gated on the ownership verdict: "Checking…" until a
    /// gather runs, then either the observer banner (a foreign/foreground/
    /// indeterminate owner — install is replaced by guidance naming the channel) or
    /// the normal install/toggle section (the daemon is the app's to manage).
    @ViewBuilder private var ownershipAwareContent: some View {
        if !agent.ownershipChecked {
            Label("Checking who manages runnyd…", systemImage: "hourglass")
                .font(.caption)
                .foregroundStyle(.secondary)
        } else if let hint = AgentController.observerMessage(for: agent.ownership) {
            observerBanner(hint)
        } else {
            installSection
        }
    }

    /// The install/toggle affordance — shown only when the daemon is the app's to
    /// manage (unmanaged or self-managed). A foreign owner gets the banner instead.
    @ViewBuilder private var installSection: some View {
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
            if canRepair {
                Button("Repair…") { confirmingRepair = true }
                    .controlSize(.small)
            }
            if let repairError = agent.repairError {
                Text(repairError)
                    .font(.caption)
                    .foregroundStyle(.red)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
    }

    /// The observer banner: names the managing channel and the operator's next step,
    /// in place of the install toggle. Icon/tint read the verdict's `kind`, never
    /// the prose.
    @ViewBuilder private func observerBanner(_ hint: ObserverHint) -> some View {
        Label {
            Text(hint.message)
                .textSelection(.enabled)
                .fixedSize(horizontal: false, vertical: true)
        } icon: {
            Image(systemName: bannerIcon(hint.kind))
        }
        .font(.caption)
        .foregroundStyle(bannerTint(hint.kind))
    }

    private func bannerIcon(_ kind: ObserverHint.Kind) -> String {
        switch kind {
        case .managedByHomebrew, .managedManually: "info.circle"
        case .foregroundDaemon, .indeterminate: "exclamationmark.triangle"
        }
    }

    private func bannerTint(_ kind: ObserverHint.Kind) -> Color {
        switch kind {
        case .managedByHomebrew, .managedManually: .secondary
        case .foregroundDaemon, .indeterminate: .orange
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
                } else if DaemonStore.uninstallNeedsConfirmation(
                    connected: store.connection == .connected, liveGuestSlots: store.liveGuestSlots
                ) {
                    confirmingUninstall = true
                } else {
                    Task { await agent.uninstall() }
                }
            }
        )
    }

    private var abandonedSlotsText: String {
        let slots = store.liveGuestSlots
        return slots.count == 1 ? "slot \(slots[0])" : "slots \(slots.joined(separator: ", "))"
    }

    private var reconcileWarning: String? {
        switch agent.reconcileState {
        case .notChecked, .ok: nil
        case let .foreign(path): "A runnyd agent is registered from an unexpected location (\(path)). "
            + (canRepair ? "Repair re-registers it from /Applications." : "Reinstall from /Applications to repoint it.")
        case .undetermined: "Couldn't determine the registered runnyd agent's location."
        }
    }

    /// The repair re-registers the canonical agent — only safe from an eligible
    /// `/Applications` bundle. A non-canonical bundle gets move-to-/Applications
    /// guidance instead (no button), so a repair can never install a second
    /// non-canonical agent. See `AgentController.canRepair`.
    private var canRepair: Bool {
        AgentController.canRepair(reconcile: agent.reconcileState, eligibility: agent.eligibility)
    }

    /// Eligibility gates only install (turning on); an installed agent stays
    /// toggle-able anywhere so a stale agent can be uninstalled even from a
    /// translocated/moved launch. See `LaunchAgentStatus.canToggle`.
    private var canToggle: Bool {
        LaunchAgentStatus.canToggle(state: agent.installState, eligibility: agent.eligibility)
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
