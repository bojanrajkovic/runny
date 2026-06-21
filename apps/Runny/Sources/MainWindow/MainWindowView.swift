import RunnyV1
import SwiftUI

enum SidebarItem: Hashable {
    case doctor
    case logs
    case slot(String)
}

struct MainWindowView: View {
    @Environment(DaemonStore.self) private var store
    @State private var selection: SidebarItem?

    var body: some View {
        @Bindable var store = store
        NavigationSplitView {
            VStack(alignment: .leading, spacing: 0) {
                DaemonCard()
                    .padding(10)
                List(selection: $selection) {
                    Section("Runners") {
                        ForEach(store.slots, id: \.slot) { slot in
                            SidebarSlotRow(slot: slot)
                                .tag(SidebarItem.slot(slot.slot))
                        }
                    }
                    Section("Daemon") {
                        Label("Logs", systemImage: "text.alignleft")
                            .imageScale(.small)
                            .tag(SidebarItem.logs)
                        Label("Doctor", systemImage: "stethoscope")
                            .imageScale(.small)
                            .tag(SidebarItem.doctor)
                    }
                }
                .listStyle(.sidebar)
            }
            // Widen the sidebar while the Local Network card shows — its 2–3 line
            // message is cramped at the default width. It never shows for a system
            // daemon (auto-allowed Local Network), so that case keeps the narrow sidebar.
            .navigationSplitViewColumnWidth(
                min: store.localNetworkCard == .hidden ? 180 : 240,
                ideal: store.localNetworkCard == .hidden ? 215 : 300
            )
        } detail: {
            switch selection {
            case .doctor:
                DoctorPane()
            case .logs:
                FleetLogsPane()
            case let .slot(name):
                if let slot = store.slots.first(where: { $0.slot == name }) {
                    SlotDetailView(slot: slot)
                        .id(name) // fresh tab/model state per slot
                } else {
                    ContentUnavailableView(
                        "Runner gone", systemImage: "questionmark.circle",
                        description: Text("That slot is no longer reported by the daemon.")
                    )
                }
            case nil:
                ContentUnavailableView(
                    "Select a runner", systemImage: "play.rectangle.on.rectangle",
                    description: Text("Pick a runner from the sidebar to see its status, cycle timeline, and logs.")
                )
            }
        }
        .commandAlerts()
        .recycleConfirmation()
        .reloadConfirmation()
    }
}

/// The two transient command channels (failure alert, advisory note) for a
/// scene root, with their nil→bool binding boilerplate in one place — the
/// alert counterpart to the popover's StatusBanners.
struct CommandAlerts: ViewModifier {
    @Environment(DaemonStore.self) private var store

    func body(content: Content) -> some View {
        content
            .alert(
                "Command failed", isPresented: binding(\.commandError),
                actions: { Button("OK") { store.commandError = nil } },
                message: { Text(store.commandError ?? "") }
            )
            .alert(
                "Heads up", isPresented: binding(\.commandNote),
                actions: { Button("OK") { store.commandNote = nil } },
                message: { Text(store.commandNote ?? "") }
            )
    }

    private func binding(_ keyPath: ReferenceWritableKeyPath<DaemonStore, String?>) -> Binding<Bool> {
        Binding(
            get: { store[keyPath: keyPath] != nil },
            set: { if !$0 { store[keyPath: keyPath] = nil } }
        )
    }
}

extension View {
    func commandAlerts() -> some View { modifier(CommandAlerts()) }
}

/// Compact daemon status: connection, version, uptime, last update.
struct DaemonCard: View {
    @Environment(DaemonStore.self) private var store

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            // Dot + "runnyd <version>" only; uptime and last-contact are
            // diagnostics, not glance data — they live in the tooltip.
            MenuBarHeader(showSubtitle: false)
            if !store.draining.isEmpty {
                Label("draining for restart: \(store.draining)", systemImage: "arrow.triangle.2.circlepath")
                    .font(.caption2)
                    .foregroundStyle(.orange)
                    .lineLimit(2)
            }
            // A standing condition, not a one-shot event — a row beside the version
            // line, never an alert (a re-popping modal would be alarm fatigue). The
            // card always shows it while connected; the popover banner is the
            // dismissible surface.
            if let skew = store.visibleSkew {
                Label(skew.text, systemImage: "exclamationmark.triangle")
                    .font(.caption2)
                    .foregroundStyle(.orange)
                    .lineLimit(3)
            }
            // Self-hides unless the agent is installed and the daemon is unreachable
            // (Start), or approval is pending (Login Items CTA).
            DaemonStartAffordance()
            // Proactive Local Network grant card — self-hides unless the daemon
            // reports an unknown/denied grant.
            LocalNetworkGrantCard()
            // Post-upgrade daemon-update affordance — self-hides unless the
            // app-installed agent is newer than the running daemon.
            DaemonUpdateAffordance()
            HStack {
                Button(store.reloadInFlight ? "Validating…" : "Reload Config…") {
                    store.requestReload()
                }
                .disabled(store.reloadInFlight || store.client == nil)
                // Manual re-dial of the same daemon at the fixed ~/.runny.
                // Disabled while a reload is draining so a re-dial can't tear
                // down the stream and discard the convergence verdict mid-drain;
                // a genuine mid-drain hang stays loud via the wedged-drain path.
                Button("Reconnect") {
                    store.restart()
                }
                .disabled(store.reloadPending)
            }
            .controlSize(.small)
            .padding(.top, 2)
        }
        .padding(8)
        .background(
            RoundedRectangle(cornerRadius: 6).fill(Color.gray.opacity(0.08)))
        // The padding/transparent areas aren't hit-testable for .help without
        // an explicit shape — without this the tooltip only fires over glyphs.
        .contentShape(Rectangle())
        .help(tooltip)
    }

    private var tooltip: String {
        var parts: [String] = []
        if let started = store.daemonStarted {
            parts.append("Up since \(Self.clock.string(from: started))")
        }
        if let last = store.lastUpdate {
            parts.append("last contact \(Self.clock.string(from: last))")
        }
        return parts.joined(separator: " · ")
    }

    static let clock: DateFormatter = {
        let formatter = DateFormatter()
        formatter.dateStyle = .none
        formatter.timeStyle = .medium
        return formatter
    }()
}

struct SidebarSlotRow: View {
    @Environment(DaemonStore.self) private var store
    let slot: Runny_V1_SlotStatus

    var body: some View {
        HStack(spacing: 7) {
            StatusIndicator(slot: slot, size: 7)
            VStack(alignment: .leading, spacing: 1) {
                // The slot name, not the runner name: it's short and stable,
                // so it never truncates in the sidebar. The full runner name
                // (long, churns every cycle) lives in the detail header/Info.
                Text(slot.slot)
                    .lineLimit(1)
                Text(SlotPresentation.statePhrase(slot))
                    .font(.caption)
                    .foregroundStyle(slot.wedged ? .red : .secondary)
                    .lineLimit(1)
            }
        }
        .contextMenu { SlotCommands(slot: slot) }
    }
}

struct DoctorPane: View {
    @Environment(DaemonStore.self) private var store

    var body: some View {
        Group {
            if let checks = store.doctorChecks {
                List(checks, id: \.name) { check in
                    DoctorRow(check: check)
                        .listRowSeparator(.hidden)
                }
                .listStyle(.inset)
                .scrollContentBackground(.hidden)
            } else {
                ContentUnavailableView(
                    "No results yet", systemImage: "stethoscope",
                    description: Text("Run the daemon's validation checks — image cache, GitHub credentials, network.")
                )
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        // Title + action live in the native window toolbar (OrbStack's pattern):
        // toolbar items stay clickable in the title-bar band and the gaps between
        // them drag the window, so the action sits right of the title without the
        // header rising under the title bar (where macOS 26.0.x eats the click).
        .navigationTitle("Doctor")
        .navigationSubtitle(doctorSubtitle)
        .toolbar {
            ToolbarItem(placement: .primaryAction) {
                Button(store.doctorRunning ? "Running…" : "Run Checks") {
                    store.runDoctor()
                }
                .disabled(store.doctorRunning || store.client == nil)
            }
        }
    }

    /// "last run … ago", coarse (no per-second tick — a toolbar subtitle is not a
    /// live surface); empty before the first run so no subtitle shows.
    private var doctorSubtitle: String {
        guard let ranAt = store.doctorRanAt else { return "" }
        return "last run \(SlotPresentation.duration(Date().timeIntervalSince(ranAt))) ago"
    }
}

/// One doctor check: status glyph, a friendly title (the wire name's hyphen
/// slug humanized), the qualifier — `runner-perm:<target>` /
/// `image-resolve:<pool>` — as a mono tag, and the detail beneath.
struct DoctorRow: View {
    let check: Runny_V1_DoctorCheck

    var body: some View {
        let parsed = SlotPresentation.doctorTitle(check.name)
        return HStack(alignment: .firstTextBaseline, spacing: 8) {
            Image(systemName: check.ok ? "checkmark.circle.fill" : "xmark.circle.fill")
                .foregroundStyle(check.ok ? Color.green : Color.red)
                .accessibilityLabel(check.ok ? "passed" : "failed")
            VStack(alignment: .leading, spacing: 1) {
                HStack(spacing: 6) {
                    Text(parsed.title)
                    if let qualifier = parsed.qualifier {
                        Text(qualifier)
                            .font(.system(.caption, design: .monospaced))
                            .foregroundStyle(.secondary)
                            .padding(.horizontal, 5)
                            .padding(.vertical, 1)
                            .background(
                                RoundedRectangle(cornerRadius: 4)
                                    .fill(Color.secondary.opacity(0.12))
                            )
                    }
                }
                if !check.detail.isEmpty {
                    Text(check.detail)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
            }
        }
    }
}
