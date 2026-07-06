import AppKit
import RunnyV1
import SwiftUI

struct SlotDetailView: View {
    @Environment(DaemonStore.self) private var store

    enum Tab: String, CaseIterable {
        case info = "Info"
        case timeline = "Timeline"
        case logs = "Logs"
    }

    let slot: Runny_V1_SlotStatus
    @State private var tab: Tab = .info

    var body: some View {
        VStack(spacing: 0) {
            header
                .padding(.horizontal)
                .padding(.top, 14)
                .padding(.bottom, 6)
            // Left-aligned, attached to the content — a centered segmented
            // control reads iPad/web; this is the native detail-pane switcher.
            HStack {
                Picker("", selection: $tab) {
                    ForEach(Tab.allCases, id: \.self) { Text($0.rawValue) }
                }
                .pickerStyle(.segmented)
                .labelsHidden()
                .fixedSize()
                Spacer()
            }
            .padding(.horizontal)
            .padding(.bottom, 8)
            Divider()
            switch tab {
            case .info: InfoTab(slot: slot)
            case .timeline: TimelineTab(slot: slot)
            case .logs: LogsTab(slotName: slot.slot)
            }
        }
        // Title + Recycle live in the native window toolbar (OrbStack's pattern):
        // toolbar items stay clickable in the title-bar band and the gaps between
        // them drag the window, so Recycle sits right of the runner name without
        // the header rising under the title bar (where macOS 26.0.x eats the
        // click). Recycle is a daemon no-op in BACKOFF (no guest), disabled there.
        .navigationTitle(SlotPresentation.displayName(slot))
        // GUI voice (statePhrase) sits beside the human runner name, so
        // "Wedged"/"Listening", never the CLI's "WEDGED!"/"LISTENING".
        .navigationSubtitle(SlotPresentation.statePhrase(slot))
        .toolbar {
            ToolbarItem(placement: .primaryAction) {
                Button("Recycle") {
                    store.requestRecycle(slot)
                }
                .disabled(slot.state == .backoff)
            }
        }
    }

    private var header: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack(spacing: 8) {
                StateBadge(slot: slot)
                if slot.paused, !slot.wedged {
                    Label("Paused", systemImage: "pause.fill")
                        .labelStyle(PausedChipStyle())
                }
                TickingText { now in
                    "for \(SlotPresentation.duration(SlotPresentation.timeInState(slot, now: now)))"
                }
                .font(.callout)
                .foregroundStyle(.secondary)
                // Only while a job is actually running: a post-job DEBUG hold
                // keeps Status.Job as history (hasJob stays true) even though
                // the runner is killed and dead — so gate on the live state,
                // not hasJob, to avoid ticking elapsed on a finished job.
                if slot.state == .job, slot.hasJob {
                    HStack(spacing: 4) {
                        Image(systemName: "hammer")
                        TickingText { now in
                            SlotPresentation.runningJob(slot, now: now)
                        }
                    }
                    .font(.callout)
                    .lineLimit(1)
                    .truncationMode(.tail)
                }
                if slot.hasVm, !slot.vm.ip.isEmpty {
                    Label(slot.vm.ip, systemImage: "network")
                        .font(.callout)
                        .monospacedDigit()
                        .foregroundStyle(.secondary)
                }
                DebugHoldChip(slot: slot)
                Spacer()
                // The in-flight command indicator moved here when the title row
                // became the toolbar; it stays visible on the detail, not only
                // the sidebar.
                if let pending = store.pendingCommand(for: slot.slot) {
                    Text(pending.displayText)
                        .font(.caption)
                        .foregroundStyle(.orange)
                }
            }
            if !SlotPresentation.note(slot, now: Date()).isEmpty {
                TickingText { now in
                    SlotPresentation.note(slot, now: now)
                }
                .font(.callout)
                .foregroundStyle(slot.lastFailure.isEmpty ? .secondary : Color.orange)
                .lineLimit(2)
                .truncationMode(.tail)
            }
        }
    }
}

/// A subdued capsule for the "Paused" header chip — paused is now textual
/// (not just the old "*"), so it survives without color.
private struct PausedChipStyle: LabelStyle {
    func makeBody(configuration: Configuration) -> some View {
        HStack(spacing: 3) {
            configuration.icon
            configuration.title
        }
        .font(.caption)
        .fontWeight(.medium)
        .padding(.horizontal, 7)
        .padding(.vertical, 2)
        .background(Capsule().fill(Color.secondary.opacity(0.18)))
        .foregroundStyle(.secondary)
    }
}

/// Operator debug-hold status: armed (a JOB that will hold at end) or a live
/// release countdown (a slot parked in DEBUG).
struct DebugHoldChip: View {
    let slot: Runny_V1_SlotStatus

    var body: some View {
        if slot.state == .debug, slot.hasDebugHoldExpires {
            TickingText { now in
                SlotPresentation.debugRelease(slot, now: now) ?? ""
            }
            .font(.callout)
            .foregroundStyle(.purple)
        } else if slot.debugHoldArmed {
            Label("debug hold armed", systemImage: "pause.circle")
                .font(.callout)
                .foregroundStyle(.purple)
        }
    }
}

struct StateBadge: View {
    let slot: Runny_V1_SlotStatus

    var body: some View {
        Text(SlotPresentation.statePhrase(slot))
            .font(.callout)
            .fontWeight(.semibold)
            .padding(.horizontal, 8)
            .padding(.vertical, 2)
            .background(Capsule().fill(slot.effectiveTint.opacity(0.18)))
            .foregroundStyle(slot.effectiveTint)
            .accessibilityLabel(SlotPresentation.statePhrase(slot))
    }
}

/// The Info card: durable identity and config facts. The live state, failure
/// note, and retry countdown that used to repeat here all live in the header
/// now — this card no longer echoes them. Custom rows (not `Form`) for tight
/// height, a mono+copyable digest, and the Paused toggle.
struct InfoTab: View {
    @Environment(DaemonStore.self) private var store
    let slot: Runny_V1_SlotStatus

    var body: some View {
        ScrollView {
            VStack(spacing: 0) {
                DetailRow(label: "Slot", value: slot.slot)
                if !slot.runnerName.isEmpty {
                    DetailRow(label: "Runner name", value: slot.runnerName, mono: true, truncate: true, copyable: true)
                }
                DetailRow(label: "Entered", value: slot.stateEntered.dateValue.formatted(date: .abbreviated, time: .standard))
                if !slot.cycleID.isEmpty {
                    DetailRow(label: "Cycle", value: slot.cycleID, mono: true, copyable: true)
                }
                if slot.hasVm {
                    if !slot.vm.ip.isEmpty { DetailRow(label: "IP", value: slot.vm.ip, mono: true) }
                    if !slot.vm.mac.isEmpty { DetailRow(label: "MAC", value: slot.vm.mac, mono: true) }
                }
                if !slot.image.isEmpty {
                    DetailRow(label: "Image", value: slot.image, truncate: true)
                }
                if !slot.imageDigest.isEmpty {
                    DetailRow(label: "Digest", value: slot.imageDigest, mono: true, truncate: true, copyable: true)
                }
                if !slot.runnerVersion.isEmpty {
                    DetailRow(label: "Runner version", value: slot.runnerVersion, truncate: true)
                }
                if slot.hasJob {
                    DetailRow(label: "Job", value: slot.job.name)
                    DetailRow(label: "Job started", value: slot.job.started.dateValue.formatted(date: .abbreviated, time: .standard))
                    if !slot.job.operatorKeys.isEmpty {
                        // Security-relevant: the job ran with an operator debug
                        // key in its trust environment.
                        DetailRow(label: "Operator keys", value: slot.job.operatorKeys.joined(separator: ", "), tint: .orange)
                    }
                }
                if slot.debugHoldArmed {
                    DetailRow(label: "Debug hold", value: "armed — enters DEBUG when the job ends", tint: .purple)
                }
                if slot.hasDebugHoldExpires {
                    DetailRow(label: "Debug hold expires", value: slot.debugHoldExpires.dateValue.formatted(date: .abbreviated, time: .standard), tint: .purple)
                }
                if slot.wedged {
                    DetailRow(label: "Wedged", value: "parked until the daemon restarts", tint: .red)
                }
                PausedRow(slot: slot)
            }
            .background(
                RoundedRectangle(cornerRadius: 8)
                    .fill(Color(nsColor: .controlBackgroundColor))
            )
            .overlay(
                RoundedRectangle(cornerRadius: 8)
                    .strokeBorder(Color(nsColor: .separatorColor), lineWidth: 0.5)
            )
            .padding()
        }
    }
}

/// One Info-card row: secondary label left, value right, tighter than a
/// `Form` row. `mono` for identifiers, `truncate` (middle) for long ones,
/// `copyable` for a hover-revealed copy button.
struct DetailRow: View {
    let label: String
    let value: String
    var mono = false
    var truncate = false
    var copyable = false
    var tint: Color?

    @State private var hovering = false

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 12) {
            Text(label)
                .foregroundStyle(.secondary)
            Spacer(minLength: 16)
            // Copy button sits to the LEFT of the value, so values stay flush
            // to the right edge on every row instead of floating off a trailing
            // gutter. The slot is reserved on all rows (copyable or not) so the
            // value never shifts when the button fades in on hover.
            ZStack(alignment: .trailing) {
                if copyable {
                    Button {
                        Pasteboard.copy(value)
                    } label: {
                        Image(systemName: "doc.on.doc").imageScale(.small)
                    }
                    .buttonStyle(.plain)
                    .foregroundStyle(.secondary)
                    .opacity(hovering ? 1 : 0)
                    .help("Copy")
                    .accessibilityLabel("Copy \(label)")
                }
            }
            .frame(width: 14)
            Text(value)
                .font(mono ? .system(.callout, design: .monospaced) : .callout)
                .foregroundStyle(tint ?? .primary)
                .multilineTextAlignment(.trailing)
                .lineLimit(truncate ? 1 : nil)
                .truncationMode(.middle)
                .textSelection(.enabled)
        }
        .font(.callout)
        .padding(.horizontal, 12)
        .padding(.vertical, 6)
        .contentShape(Rectangle())
        .onHover { hovering = $0 }
        .overlay(alignment: .bottom) {
            Divider().padding(.leading, 12)
        }
    }
}

/// The Paused row, now a toggle: flipping it on pauses after this cycle,
/// off resumes — the same store commands the menus call, surfaced inline.
struct PausedRow: View {
    @Environment(DaemonStore.self) private var store
    let slot: Runny_V1_SlotStatus

    var body: some View {
        HStack(alignment: .center) {
            VStack(alignment: .leading, spacing: 1) {
                Text("Paused")
                Text("takes effect after the current cycle")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            Spacer(minLength: 16)
            Toggle("", isOn: binding)
                .labelsHidden()
                .toggleStyle(.switch)
                .controlSize(.small)
                .disabled(store.client == nil || store.pendingCommand(for: slot.slot) != nil)
        }
        .font(.callout)
        .padding(.horizontal, 12)
        .padding(.vertical, 6)
    }

    private var binding: Binding<Bool> {
        Binding(
            get: { slot.paused },
            set: { wantsPause in
                if wantsPause { store.pauseSlot(slot) } else { store.resumeSlot(slot) }
            }
        )
    }
}
