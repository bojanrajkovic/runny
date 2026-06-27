import ServiceManagement
import SwiftUI

/// The app's Settings surface. The app is non-privileged — it never installs the
/// CLI or manages a system daemon (both raise an admin prompt), so Settings is the
/// per-user daemon's start-at-login row and nothing else. State is read from
/// `AgentController`, never an action's return.
struct SettingsView: View {
    @Environment(AgentController.self) private var agent
    @Environment(ActivationCoordinator.self) private var activation

    var body: some View {
        Form {
            Section {
                AgentInstallRow()
            } header: {
                Text("Daemon")
            } footer: {
                Text("Installs runnyd as a login agent so it starts with your session and survives "
                    + "upgrades. Requires Runny in your Applications folder.")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
        }
        .formStyle(.grouped)
        .frame(width: 480, height: 320)
        .onAppear {
            // Settings is a regular window; keep the accessory↔regular dance sane
            // (the app is LSUIElement) and refresh the agent row from its source on open.
            activation.windowAppeared()
            agent.refresh()
            Task { await agent.runReconcile() }
        }
    }
}

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
    /// Default-on. When on, a surface-driven trigger auto-applies an OK update
    /// without the button (Warn/Error still drop to the manual affordance).
    @AppStorage(Prefs.autoApplyDaemonUpdates) private var autoApplyDaemonUpdates = true

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
            Button("Repair", role: repairNeedsAbandonWarning ? .destructive : nil) {
                Task { await agent.repair() }
            }
            Button("Cancel", role: .cancel) {}
        } message: {
            // Repair briefly unregisters the agent, which can evict a running daemon on
            // some macOS versions — so it carries the same abandon warning uninstall does
            // whenever a job may be live, INCLUDING the disconnected-unknown case (an
            // empty slot list while the daemon isn't connected is "can't prove nothing is
            // running"). The spawn gate + observer banner keep a foreign daemon out of
            // this path, so it is only ever the app's own stale agent here.
            if !repairNeedsAbandonWarning {
                Text("A runnyd agent is registered from an unexpected location. Repair re-registers "
                    + "Runny's bundled daemon under the launchd agent “\(SMAppServiceRegistrar.agentLabel)”, "
                    + "briefly removing and re-adding it to re-point it at this app.")
            } else if store.liveGuestSlots.isEmpty {
                Text("Repair briefly removes and re-adds the login agent. Runny can't confirm whether a "
                    + "job is running because the daemon isn't connected, so this may abandon a running "
                    + "job or a debug-held guest.")
            } else {
                Text("Repair briefly removes and re-adds the login agent to re-point it. A job is "
                    + "running on \(abandonedSlotsText) — removing the agent may stop runnyd and abandon "
                    + "that work, which will not finish, and a debug-held guest is destroyed.")
            }
        }
    }

    /// Repair unregisters the agent, which can evict a running daemon — so it warns
    /// (and goes destructive) whenever uninstall would: a live job, OR a disconnected
    /// daemon whose guest state can't be proven empty. The same connection-aware check.
    private var repairNeedsAbandonWarning: Bool {
        DaemonStore.uninstallNeedsConfirmation(
            connected: store.connection == .connected, liveGuestSlots: store.liveGuestSlots
        )
    }

    /// The Daemon row content, gated on the ownership verdict: "Checking…" until a
    /// gather runs, then either the observer banner (a systemManaged or indeterminate
    /// owner — install is replaced by guidance) or the normal install/toggle section
    /// (the daemon is the app's to manage, i.e. unmanaged/selfManaged/awaitingApproval).
    @ViewBuilder private var ownershipAwareContent: some View {
        if !agent.ownershipChecked {
            Label("Checking who manages runnyd…", systemImage: "hourglass")
                .font(.caption)
                .foregroundStyle(.secondary)
        } else {
            if let hint = AgentController.observerMessage(for: agent.ownership) {
                observerBanner(hint)
            } else {
                installSection
            }
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
        // Auto-apply only applies to the per-user agent the app drain-respawns, so it's
        // offered only once that agent is installed. Off = today's button-only behavior.
        if agent.installState == .installed {
            Toggle("Automatically apply runnyd upgrades", isOn: $autoApplyDaemonUpdates)
            Text("When a newer runnyd ships with the app and accepts your config, apply it on open — draining running jobs first. Warnings or errors wait for you.")
                .font(.caption)
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
        }
        if agent.installState == .requiresApproval {
            // Re-gather before sending the user to approve: approval launches the
            // RunAtLoad agent OUTSIDE the spawn gate, so a foreign owner that appeared
            // since render must suppress this rather than direct an approval that creates
            // a competing manager. revalidate publishes the fresh verdict, flipping the
            // surface to the observer banner when it no longer reads awaitingApproval.
            Button("Open Login Items…") {
                Task { if await agent.revalidate(.awaitingApproval) { SMAppService.openSystemSettingsLoginItems() } }
            }
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
        }
        // Rendered INDEPENDENTLY of the reconcile warning: a repair that fails after the
        // unregister took clears the warning (the agent is gone → reconcile .ok), so a
        // repairError nested under it would vanish exactly when the repair failed — a
        // silent failure in the recovery path. Keep it loud on its own.
        if let repairError = agent.repairError {
            Text(repairError)
                .font(.caption)
                .foregroundStyle(.red)
                .fixedSize(horizontal: false, vertical: true)
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
        case .systemManaged: "info.circle"
        case .indeterminate: "exclamationmark.triangle"
        }
    }

    private func bannerTint(_ kind: ObserverHint.Kind) -> Color {
        switch kind {
        case .systemManaged: .secondary
        case .indeterminate: .orange
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
                } else {
                    requestUninstall()
                }
            }
        )
    }

    /// Remove the app's agent through the live-guest-aware path: raise the abandon
    /// confirmation when a job may be live (or the daemon is disconnected and we can't
    /// prove otherwise), else uninstall directly. The toggle's OFF path routes through
    /// here so it never bypasses the warning.
    private func requestUninstall() {
        // A pending (`.requiresApproval`) agent is registered but never started, so it
        // can't be the daemon serving any job — removing it abandons nothing. Skip the
        // abandon confirmation, whose text describes stopping a SERVING daemon: under a
        // foreign owner the connected daemon (and its live slots) belongs to that owner,
        // not our pending agent, so that warning would be false. An `.installed` agent
        // could be the one holding the socket, so it still confirms conservatively.
        if agent.installState != .requiresApproval,
           DaemonStore.uninstallNeedsConfirmation(
               connected: store.connection == .connected, liveGuestSlots: store.liveGuestSlots
           )
        {
            confirmingUninstall = true
        } else {
            Task { await agent.uninstall() }
        }
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
