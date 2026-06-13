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
            if !store.draining.isEmpty {
                StatusBanner(
                    text: "draining for restart: \(store.draining)",
                    systemImage: "arrow.triangle.2.circlepath", tint: .orange
                )
            }
            // Command failures must be visible HERE: the main window's alert
            // doesn't exist while only the popover is open, and a recycle
            // that fails invisibly is a silent failure.
            if let error = store.commandError {
                StatusBanner(text: error, systemImage: "exclamationmark.triangle.fill",
                             tint: .red) { store.commandError = nil }
            }
            if let note = store.commandNote {
                StatusBanner(text: note, systemImage: "info.circle.fill",
                             tint: .blue) { store.commandNote = nil }
            }
            Divider()
            if store.slots.isEmpty {
                emptyState
            } else {
                ScrollView {
                    VStack(spacing: 2) {
                        ForEach(store.slots, id: \.slot) { slot in
                            MenuBarSlotRow(slot: slot)
                        }
                    }
                    .padding(.vertical, 6)
                    .padding(.horizontal, Metrics.pad / 2)
                }
                // Cap the runner list so a many-slot host can't grow the
                // popover past the screen and push Quit / Open Runny off the
                // bottom; beyond the cap it scrolls. Sized to content below it.
                .frame(maxHeight: CGFloat(min(store.slots.count, 8)) * 46 + 12)
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

/// One banner vocabulary for drain/error/note across the popover. A nil
/// dismiss closure renders a non-dismissible banner (the drain state, which
/// clears itself from snapshots).
struct StatusBanner: View {
    let text: String
    let systemImage: String
    let tint: Color
    var dismiss: (() -> Void)?

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 6) {
            Image(systemName: systemImage)
                .foregroundStyle(tint)
                .font(.caption)
            Text(text)
                .font(.caption)
                .foregroundStyle(.primary)
                .lineLimit(3)
            Spacer(minLength: 4)
            if let dismiss {
                Button(action: dismiss) {
                    Image(systemName: "xmark.circle.fill")
                        .foregroundStyle(.secondary)
                }
                .buttonStyle(.plain)
            }
        }
        .padding(.horizontal, Metrics.pad)
        .padding(.vertical, 6)
        .background(tint.opacity(0.08))
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
    /// The sidebar card hides the uptime line (it lives in the card's tooltip
    /// instead); the popover keeps it.
    var showSubtitle = true

    var body: some View {
        HStack(spacing: 8) {
            Circle()
                .fill(dotColor)
                .frame(width: Metrics.statusDot, height: Metrics.statusDot)
                .accessibilityLabel("Connection")
                .accessibilityValue(title)
            VStack(alignment: .leading, spacing: 1) {
                Text(title)
                    .font(.callout)
                    .fontWeight(.medium)
                    .lineLimit(1)
                    .truncationMode(.tail)
                if showSubtitle {
                    subtitle
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

    /// Ages tick — a frozen "up 3h" reads as live data and lies.
    @ViewBuilder
    private var subtitle: some View {
        switch store.connection {
        case .connected:
            if let started = store.daemonStarted {
                TickingText { now in
                    "up \(SlotPresentation.duration(now.timeIntervalSince(started)))"
                }
            }
        case let .stale(since):
            TickingText { now in
                "last update \(SlotPresentation.duration(now.timeIntervalSince(since))) ago"
            }
        case let .unreachable(reason):
            Text(reason)
        case .connecting, .reconnecting:
            EmptyView()
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
                StatusIndicator(slot: slot, size: 6)
                Text(SlotPresentation.displayName(slot))
                    .font(.callout)
                    .fontWeight(.medium)
                    .lineLimit(1)
                    .truncationMode(.middle)
                Spacer(minLength: 4)
                // Only the elapsed digits tick, not the whole row.
                TickingText { now in
                    "\(SlotPresentation.statePhrase(slot)) · \(SlotPresentation.duration(SlotPresentation.timeInState(slot, now: now)))"
                }
                .font(.caption)
                .foregroundStyle(slot.wedged ? .red : Metrics.secondaryText)
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
            TickingText { now in
                SlotPresentation.runningJob(slot, now: now)
            }
            .font(.caption)
            .foregroundStyle(.blue)
            .lineLimit(1)
            .truncationMode(.tail)
        } else if !noteText(now: Date()).isEmpty {
            // Emptiness only changes with snapshots, so the gate doesn't
            // need to tick; the countdown inside does.
            TickingText { now in
                noteText(now: now)
            }
            .font(.caption)
            .foregroundStyle(.secondary)
            .lineLimit(1)
            .truncationMode(.tail)
        }
    }

    private func noteText(now: Date) -> String {
        if let pending = store.pendingCommand(for: slot.slot) {
            return pending.displayText
        }
        if let release = SlotPresentation.debugRelease(slot, now: now) {
            return "debug hold — \(release)"
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
            store.requestRecycle(slot)
            // The confirmation dialog lives only on the main window (one
            // presenter), so a force-recycle from the popover needs the window
            // up to confirm; a plain recycle runs without one.
            if openInApp, store.recycleNeedsConsent(slot) {
                activation.openMainWindow(openWindow)
            }
        }
        // Recycle is a daemon no-op in BACKOFF (no guest to recycle).
        .disabled(slot.state == .backoff)
        if openInApp {
            Divider()
            Button("Open in Runny") {
                activation.openMainWindow(openWindow)
            }
        }
    }
}

/// Hosts the recycle confirmation for the `-force` cases (cancel a running
/// job, destroy a debug hold). Applied at each scene root so whichever
/// surface is frontmost presents it; everything else recycles one-click.
struct RecycleConfirmation: ViewModifier {
    @Environment(DaemonStore.self) private var store

    func body(content: Content) -> some View {
        content.confirmationDialog(
            title, isPresented: isPresented, presenting: store.recycleConfirm
        ) { slot in
            Button(actionLabel(slot), role: .destructive) {
                store.confirmRecycle(slot)
            }
        } message: { slot in
            Text(message(slot))
        }
    }

    private var isPresented: Binding<Bool> {
        Binding(
            get: { store.recycleConfirm != nil },
            set: { if !$0 { store.recycleConfirm = nil } }
        )
    }

    private var title: String {
        guard let slot = store.recycleConfirm else { return "" }
        return "Recycle \(SlotPresentation.displayName(slot))?"
    }

    private func actionLabel(_ slot: Runny_V1_SlotStatus) -> String {
        slot.state == .job ? "Cancel Job & Recycle" : "Release Hold & Recycle"
    }

    private func message(_ slot: Runny_V1_SlotStatus) -> String {
        if slot.state == .job {
            let name = slot.job.name.isEmpty ? "the running job" : "job “\(slot.job.name)”"
            return "Recycling cancels \(name) and tears the runner down."
        }
        return "This destroys the held debug guest and starts a fresh cycle."
    }
}

extension View {
    func recycleConfirmation() -> some View { modifier(RecycleConfirmation()) }
}

struct DoctorChip: View {
    @Environment(DaemonStore.self) private var store

    var body: some View {
        // Cached last result + age only — Doctor re-runs full validation
        // (GitHub API calls included); the main window owns re-runs.
        Group {
            if let checks = store.doctorChecks, let ranAt = store.doctorRanAt {
                let failed = checks.count(where: { !$0.ok })
                TickingText { now in
                    let age = SlotPresentation.duration(now.timeIntervalSince(ranAt))
                    return failed == 0 ? "✓ \(age) ago" : "\(failed) failed · \(age) ago"
                }
                .foregroundStyle(failed == 0 ? Color.green : Color.red)
            } else {
                Text("doctor —")
                    .foregroundStyle(.secondary)
            }
        }
        .font(.caption)
        .help("Last doctor run; re-run from the main window")
    }
}
