import RunnyV1
import SwiftUI

/// The popover: a visual `runnyctl status`, scannable at a glance.
struct MenuBarView: View {
    @Environment(DaemonStore.self) private var store
    @Environment(ActivationCoordinator.self) private var activation
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            MenuBarHeader()
                .padding(Metrics.pad)
            Divider()
            if store.slots.isEmpty {
                emptyState
            } else {
                VStack(spacing: 2) {
                    ForEach(store.slots, id: \.slot) { slot in
                        MenuBarSlotRow(slot: slot)
                    }
                }
                .padding(.vertical, 6)
                .padding(.horizontal, Metrics.pad / 2)
            }
            Divider()
            footer
                .padding(Metrics.pad)
        }
        .frame(width: Metrics.popoverWidth)
        .onAppear { store.start() }
    }

    private var emptyState: some View {
        Text(emptyText)
            .font(.callout)
            .foregroundStyle(.secondary)
            .frame(maxWidth: .infinity, alignment: .center)
            .padding(.vertical, 20)
    }

    private var emptyText: String {
        if case .connected = store.connection { return "no slots configured" }
        return "waiting for the daemon"
    }

    private var footer: some View {
        HStack {
            Button("Open Runny") {
                activation.openMainWindow(openWindow)
            }
            Spacer()
            DoctorChip()
            Spacer()
            Button("Quit") { NSApp.terminate(nil) }
        }
        .controlSize(.small)
    }
}

enum Metrics {
    static let popoverWidth: CGFloat = 360
    static let pad: CGFloat = 12
    static let statusDot: CGFloat = 8
    static let rowCorner: CGFloat = 5
    static let secondaryText = Color.primary.opacity(0.72)
}

struct MenuBarHeader: View {
    @Environment(DaemonStore.self) private var store

    var body: some View {
        HStack(spacing: 8) {
            Circle()
                .fill(dotColor)
                .frame(width: Metrics.statusDot, height: Metrics.statusDot)
            VStack(alignment: .leading, spacing: 1) {
                Text(title)
                    .font(.callout)
                    .fontWeight(.medium)
                    .lineLimit(1)
                    .truncationMode(.tail)
                if let subtitle {
                    Text(subtitle)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .lineLimit(2)
                }
            }
            Spacer(minLength: 0)
        }
    }

    private var dotColor: Color {
        switch store.connection {
        case .connected: .green
        case .connecting, .reconnecting: .orange
        case .stale: .orange
        case .unreachable: .secondary
        }
    }

    private var title: String {
        switch store.connection {
        case .connected:
            store.daemonVersion.isEmpty
                ? "runnyd" : "runnyd \(store.daemonVersion)"
        case .connecting: "connecting…"
        case .reconnecting: "reconnecting…"
        case .stale: "daemon not responding"
        case .unreachable: "daemon unreachable"
        }
    }

    private var subtitle: String? {
        switch store.connection {
        case .connected:
            guard let started = store.daemonStarted else { return nil }
            return "up \(SlotPresentation.duration(Date().timeIntervalSince(started)))"
        case let .stale(since):
            return "last update \(SlotPresentation.duration(Date().timeIntervalSince(since))) ago"
        case let .unreachable(reason):
            return reason
        case .connecting, .reconnecting:
            return nil
        }
    }
}

struct MenuBarSlotRow: View {
    @Environment(DaemonStore.self) private var store
    @Environment(ActivationCoordinator.self) private var activation
    @Environment(\.openWindow) private var openWindow
    @State private var isHovered = false

    let slot: Runny_V1_SlotStatus

    var body: some View {
        VStack(alignment: .leading, spacing: 2) {
            HStack(spacing: 6) {
                Circle()
                    .fill(slot.wedged ? Color.red : slot.state.tint)
                    .frame(width: 6, height: 6)
                Text(SlotPresentation.displayName(slot))
                    .font(.callout)
                    .fontWeight(.medium)
                    .lineLimit(1)
                    .truncationMode(.tail)
                Spacer(minLength: 4)
                // Only the elapsed digits tick, not the whole row.
                TimelineView(.periodic(from: .now, by: 1)) { context in
                    Text("\(SlotPresentation.stateLabel(slot)) · \(SlotPresentation.duration(SlotPresentation.timeInState(slot, now: context.date)))")
                        .font(.caption)
                        .monospacedDigit()
                        .foregroundStyle(slot.wedged ? .red : Metrics.secondaryText)
                }
            }
            secondLine
                .padding(.leading, 12)
        }
        .padding(.vertical, 4)
        .padding(.horizontal, 6)
        .background(
            RoundedRectangle(cornerRadius: Metrics.rowCorner)
                .fill(isHovered ? Color.gray.opacity(0.08) : Color.clear)
        )
        .onHover { isHovered = $0 }
        .contextMenu { SlotCommands(slot: slot, openInApp: true) }
    }

    @ViewBuilder
    private var secondLine: some View {
        if slot.state == .job, slot.hasJob {
            // The glance question while a job runs is "which job, how long".
            TimelineView(.periodic(from: .now, by: 1)) { context in
                Text("\(slot.job.name) · \(SlotPresentation.duration(context.date.timeIntervalSince(slot.job.started.dateValue)))")
                    .font(.caption)
                    .monospacedDigit()
                    .foregroundStyle(.blue)
                    .lineLimit(1)
                    .truncationMode(.tail)
            }
        } else {
            TimelineView(.periodic(from: .now, by: 1)) { context in
                let note = noteText(now: context.date)
                if !note.isEmpty {
                    Text(note)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .lineLimit(1)
                        .truncationMode(.tail)
                }
            }
        }
    }

    private func noteText(now: Date) -> String {
        if let pending = store.pendingCommand(for: slot.slot) {
            return "\(pending.kind.rawValue) requested…"
        }
        return SlotPresentation.note(slot, now: now)
    }
}

/// Shared command set: popover context menu, sidebar context menu, and the
/// detail toolbar all wire the same store methods.
struct SlotCommands: View {
    @Environment(DaemonStore.self) private var store
    @Environment(ActivationCoordinator.self) private var activation
    @Environment(\.openWindow) private var openWindow

    let slot: Runny_V1_SlotStatus
    var openInApp = false

    var body: some View {
        if slot.paused {
            Button("Resume") { store.resumeSlot(slot) }
        } else {
            Button("Pause After This Cycle") { store.pauseSlot(slot) }
        }
        Button("Recycle") {
            store.recycleSlot(slot, reason: "operator request (Runny)")
        }
        if openInApp {
            Divider()
            Button("Open in Runny") {
                activation.openMainWindow(openWindow)
            }
        }
    }
}

struct DoctorChip: View {
    @Environment(DaemonStore.self) private var store

    var body: some View {
        // Cached last result + age only — Doctor re-runs full validation
        // (GitHub API calls included); the main window owns re-runs.
        Group {
            if let checks = store.doctorChecks, let ranAt = store.doctorRanAt {
                let failed = checks.count(where: { !$0.ok })
                Text(chipText(failed: failed, ranAt: ranAt))
                    .foregroundStyle(failed == 0 ? Color.green : Color.red)
            } else {
                Text("doctor —")
                    .foregroundStyle(.secondary)
            }
        }
        .font(.caption)
        .help("Last doctor run; re-run from the main window")
    }

    private func chipText(failed: Int, ranAt: Date) -> String {
        let age = SlotPresentation.duration(Date().timeIntervalSince(ranAt))
        return failed == 0 ? "✓ \(age) ago" : "\(failed) failed · \(age) ago"
    }
}
