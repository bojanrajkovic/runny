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
            .navigationSplitViewColumnWidth(min: 180, ideal: 215)
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
            Button(store.reloadInFlight ? "Validating…" : "Reload Config…") {
                store.requestReload()
            }
            .controlSize(.small)
            .disabled(store.reloadInFlight || store.client == nil)
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

/// The shared detail-pane header band: a title with an optional inline
/// subtitle and trailing accessory, the standard top-of-pane paddings, and the
/// divider. One place owns the band so the panes can't drift.
struct PaneHeader<Subtitle: View, Trailing: View>: View {
    let title: String
    @ViewBuilder var subtitle: () -> Subtitle
    @ViewBuilder var trailing: () -> Trailing

    init(
        _ title: String,
        @ViewBuilder subtitle: @escaping () -> Subtitle = { EmptyView() },
        @ViewBuilder trailing: @escaping () -> Trailing = { EmptyView() }
    ) {
        self.title = title
        self.subtitle = subtitle
        self.trailing = trailing
    }

    var body: some View {
        VStack(spacing: 0) {
            HStack(spacing: 8) {
                Text(title)
                    .font(.title2)
                    .fontWeight(.semibold)
                subtitle()
                Spacer()
                trailing()
            }
            .padding(.horizontal)
            .padding(.top, 14)
            .padding(.bottom, 6)
            Divider()
        }
    }
}

struct DoctorPane: View {
    @Environment(DaemonStore.self) private var store

    var body: some View {
        VStack(spacing: 0) {
            PaneHeader("Doctor") {
                if let ranAt = store.doctorRanAt {
                    TickingText { now in
                        "last run \(SlotPresentation.duration(now.timeIntervalSince(ranAt))) ago"
                    }
                    .font(.caption)
                    .foregroundStyle(.secondary)
                }
            } trailing: {
                Button(store.doctorRunning ? "Running…" : "Run Checks") {
                    store.runDoctor()
                }
                .disabled(store.doctorRunning || store.client == nil)
            }
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
            // Fill below the header so the empty state centers h/v and the
            // list fills — the header stays pinned at the top either way.
            .frame(maxWidth: .infinity, maxHeight: .infinity)
        }
        .ignoresSafeArea(.container, edges: .top)
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
