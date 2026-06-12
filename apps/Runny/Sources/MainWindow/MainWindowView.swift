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
                            .tag(SidebarItem.logs)
                        Label("Doctor", systemImage: "stethoscope")
                            .tag(SidebarItem.doctor)
                    }
                }
                .listStyle(.sidebar)
            }
            .navigationSplitViewColumnWidth(min: 200, ideal: 230)
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
        .alert(
            "Command failed", isPresented: commandErrorBinding,
            actions: { Button("OK") { store.commandError = nil } },
            message: { Text(store.commandError ?? "") }
        )
    }

    private var commandErrorBinding: Binding<Bool> {
        Binding(
            get: { store.commandError != nil },
            set: { if !$0 { store.commandError = nil } }
        )
    }
}

/// Compact daemon status: connection, version, uptime, last update.
struct DaemonCard: View {
    @Environment(DaemonStore.self) private var store

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            MenuBarHeader()
            if case .connected = store.connection, let last = store.lastUpdate {
                TimelineView(.periodic(from: .now, by: 1)) { context in
                    Text("updated \(SlotPresentation.duration(context.date.timeIntervalSince(last))) ago")
                        .font(.caption2)
                        .monospacedDigit()
                        .foregroundStyle(.tertiary)
                }
            }
        }
        .padding(8)
        .background(
            RoundedRectangle(cornerRadius: 6).fill(Color.gray.opacity(0.08)))
    }
}

struct SidebarSlotRow: View {
    @Environment(DaemonStore.self) private var store
    let slot: Runny_V1_SlotStatus

    var body: some View {
        HStack(spacing: 6) {
            Circle()
                .fill(slot.wedged ? Color.red : slot.state.tint)
                .frame(width: 7, height: 7)
            VStack(alignment: .leading, spacing: 1) {
                Text(SlotPresentation.displayName(slot))
                    .lineLimit(1)
                    .truncationMode(.tail)
                Text(SlotPresentation.stateLabel(slot))
                    .font(.caption)
                    .foregroundStyle(slot.wedged ? .red : .secondary)
            }
        }
        .contextMenu { SlotCommands(slot: slot) }
    }
}

struct DoctorPane: View {
    @Environment(DaemonStore.self) private var store

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack {
                Text("Doctor")
                    .font(.title2)
                    .fontWeight(.semibold)
                if let ranAt = store.doctorRanAt {
                    Text("last run \(SlotPresentation.duration(Date().timeIntervalSince(ranAt))) ago")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
                Spacer()
                Button(store.doctorRunning ? "Running…" : "Run Checks") {
                    store.runDoctor()
                }
                .disabled(store.doctorRunning || store.client == nil)
            }
            .padding()
            Divider()
            if let checks = store.doctorChecks {
                List(checks, id: \.name) { check in
                    HStack(alignment: .firstTextBaseline) {
                        Image(systemName: check.ok ? "checkmark.circle.fill" : "xmark.circle.fill")
                            .foregroundStyle(check.ok ? Color.green : Color.red)
                        VStack(alignment: .leading, spacing: 1) {
                            Text(check.name)
                            if !check.detail.isEmpty {
                                Text(check.detail)
                                    .font(.caption)
                                    .foregroundStyle(.secondary)
                            }
                        }
                    }
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
    }
}
