import SwiftUI

/// The app's Settings surface. After the home became fixed (no override field),
/// its one job is vending the CLI: install, repair, or remove the
/// `/usr/local/bin/runnyctl` symlink. State is read from `CLIInstallModel` —
/// never an action's return — so the row always reflects what is on disk.
struct SettingsView: View {
    @Environment(CLIInstallModel.self) private var cli
    @Environment(SystemDaemonInstaller.self) private var systemDaemon
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
            Section {
                SystemDaemonInstallRow()
            } header: {
                Text("System Service (advanced)")
            } footer: {
                Text("Installs runnyd as a non-root system service that starts at boot and keeps "
                    + "running after you log out — no GUI session needed. One admin prompt creates a "
                    + "dedicated service account and the daemon; afterward write config to "
                    + "/Library/Application Support/runny. Use this OR the login agent above, not both.")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            Section {
                CLIInstallRow()
            } header: {
                Text("Command-Line Tool")
            } footer: {
                Text("Vends runnyctl at /usr/local/bin/runnyctl so you can drive the daemon "
                    + "from any terminal. The app and the CLI it installs are the same build.")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
        }
        .formStyle(.grouped)
        .frame(width: 480, height: 320)
        .onAppear {
            // Settings is a regular window; keep the accessory↔regular dance sane
            // (the app is LSUIElement) and refresh both rows from their sources on open.
            activation.windowAppeared()
            cli.refresh()
            agent.refresh()
            Task { await agent.runReconcile() }
            Task { await systemDaemon.refresh() }
        }
    }
}

/// One row that renders every `SystemDaemonInstaller.State` — install or remove the
/// non-root system LaunchDaemon via a single admin prompt, with a loud status line
/// for the translocated and failed states. State is read from the installer (the
/// launchd `system/` probe), never an action's return.
struct SystemDaemonInstallRow: View {
    @Environment(SystemDaemonInstaller.self) private var systemDaemon

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 16) {
            VStack(alignment: .leading, spacing: 4) {
                Text(headline).font(.body)
                if let detail {
                    Text(detail)
                        .font(.caption)
                        .foregroundStyle(tint)
                        .textSelection(.enabled)
                        .fixedSize(horizontal: false, vertical: true)
                }
            }
            Spacer(minLength: 0)
            actions
        }
        .padding(.vertical, 2)
    }

    private var headline: String {
        switch systemDaemon.state {
        case .notInstalled: "Not installed"
        case .installing: "Installing…"
        case .installed: "Installed"
        case .translocated: "Move Runny to Applications first"
        case .failed: "Install failed"
        case .cancelled: "Cancelled"
        }
    }

    private var detail: String? {
        switch systemDaemon.state {
        case .notInstalled:
            "Run runnyd at boot, surviving logout — no GUI login needed."
        case .installing, .cancelled:
            nil
        case .installed:
            "Runs as the _runny service account. Write config to "
                + "/Library/Application Support/runny/config.yaml to start it."
        case .translocated:
            "Run Runny from /Applications (or ~/Applications), then install — a daemon staged "
                + "from a translocated copy breaks on the next launch."
        case let .failed(message):
            message
        }
    }

    private var tint: Color {
        switch systemDaemon.state {
        case .installed, .notInstalled, .installing, .cancelled: .secondary
        case .translocated: .orange
        case .failed: .red
        }
    }

    @ViewBuilder
    private var actions: some View {
        switch systemDaemon.state {
        case .notInstalled, .cancelled:
            Button("Install…") { systemDaemon.install() }
        case .installing:
            HStack(spacing: 8) {
                ProgressView().controlSize(.small)
                Button("Cancel") { systemDaemon.cancel() }
            }
        case .installed:
            Button("Remove…") { systemDaemon.uninstall() }
        case .translocated:
            Button("Re-check") { Task { await systemDaemon.refresh() } }
        case .failed:
            Button("Try Again") { systemDaemon.install() }
        }
    }
}

/// One row that renders every `CLIInstallModel.State` — the install/repair/remove
/// affordance and a same-loud status line for the conflict, translocated, and
/// failed states (none of which are silently swallowed).
struct CLIInstallRow: View {
    @Environment(CLIInstallModel.self) private var cli

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 16) {
            VStack(alignment: .leading, spacing: 4) {
                Text(headline).font(.body)
                if let detail {
                    Text(detail)
                        .font(.caption)
                        .foregroundStyle(tint)
                        .textSelection(.enabled)
                        .fixedSize(horizontal: false, vertical: true)
                }
            }
            Spacer(minLength: 0)
            actions
        }
        .padding(.vertical, 2)
    }

    private var headline: String {
        switch cli.state {
        case .notInstalled: "Not installed"
        case .installing: "Installing…"
        case .installed: "Installed"
        case .installedButNotOnPath: "Installed — not on your PATH"
        case .orphaned: "Leftover runnyctl link"
        case let .conflict(owner): conflictGuidance(owner).headline
        case .translocated: "Move Runny to Applications first"
        case .failed: "Install failed"
        case .cancelled: "Cancelled"
        }
    }

    /// Channel-aware wording for a foreign-owner conflict: names the managing
    /// channel (Homebrew, a hand-rolled link, a dropped file) and the remediation,
    /// rather than only the raw path. See `CLIInstall.conflictGuidance`.
    private func conflictGuidance(_ owner: String) -> (headline: String, detail: String) {
        CLIInstall.conflictGuidance(
            channel: CLIInstall.foreignChannel(owner: owner, linkPath: CLIInstallModel.linkPath),
            owner: owner, linkPath: CLIInstallModel.linkPath
        )
    }

    private var detail: String? {
        switch cli.state {
        case .notInstalled:
            "Run runnyctl from any terminal without a path."
        case .installing, .cancelled:
            nil
        case .installed:
            CLIInstallModel.linkPath
        case .installedButNotOnPath:
            "/usr/local/bin isn't on your PATH — add it so runnyctl resolves."
        case let .orphaned(target):
            "\(CLIInstallModel.linkPath) → \(target), a Runny that's no longer installed. "
                + "Remove the leftover link, or Install to point it at this copy."
        case let .conflict(owner):
            conflictGuidance(owner).detail
        case .translocated:
            "Run Runny from /Applications (or ~/Applications), then install — a link into a "
                + "translocated copy breaks on the next launch."
        case let .failed(message):
            message
        }
    }

    private var tint: Color {
        switch cli.state {
        case .installed, .notInstalled, .installing, .cancelled: .secondary
        case .installedButNotOnPath, .conflict, .translocated, .orphaned: .orange
        case .failed: .red
        }
    }

    @ViewBuilder
    private var actions: some View {
        switch cli.state {
        case .notInstalled, .cancelled:
            Button("Install") { cli.install() }
        case .installing:
            HStack(spacing: 8) {
                ProgressView().controlSize(.small)
                Button("Cancel") { cli.cancel() }
            }
        case .installed, .installedButNotOnPath:
            Button("Remove") { cli.uninstall() }
        case .orphaned:
            // Clean up the dangling leftover, or adopt the path onto this copy.
            HStack(spacing: 8) {
                Button("Remove") { cli.removeOrphan() }
                Button("Install") { cli.install() }
            }
        case .conflict, .translocated:
            Button("Re-check") { cli.refresh() }
        case .failed:
            Button("Try Again") { cli.install() }
        }
    }
}
