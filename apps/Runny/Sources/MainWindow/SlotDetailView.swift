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
                .padding()
            Picker("", selection: $tab) {
                ForEach(Tab.allCases, id: \.self) { Text($0.rawValue) }
            }
            .pickerStyle(.segmented)
            .labelsHidden()
            .padding(.horizontal)
            .padding(.bottom, 8)
            Divider()
            switch tab {
            case .info: InfoTab(slot: slot)
            case .timeline: TimelineTab(slot: slot)
            case .logs: LogsTab(slotName: slot.slot)
            }
        }
    }

    private var header: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack(alignment: .firstTextBaseline) {
                Text(SlotPresentation.displayName(slot))
                    .font(.title2)
                    .fontWeight(.semibold)
                    .lineLimit(1)
                    .truncationMode(.middle)
                Spacer()
                if let pending = store.pendingCommand(for: slot.slot) {
                    Text(pending.displayText)
                        .font(.caption)
                        .foregroundStyle(.orange)
                }
                SlotCommands(slot: slot)
                    .buttonStyle(.bordered)
                    .controlSize(.small)
            }
            HStack(spacing: 8) {
                StateBadge(slot: slot)
                TickingText { now in
                    "for \(SlotPresentation.duration(SlotPresentation.timeInState(slot, now: now)))"
                }
                .font(.callout)
                .foregroundStyle(.secondary)
                if slot.hasJob {
                    HStack(spacing: 4) {
                        Image(systemName: "hammer")
                        TickingText { now in
                            "\(slot.job.name) · \(SlotPresentation.duration(now.timeIntervalSince(slot.job.started.dateValue)))"
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
                Spacer()
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

struct StateBadge: View {
    let slot: Runny_V1_SlotStatus

    var body: some View {
        Text(SlotPresentation.stateLabel(slot))
            .font(.callout)
            .fontWeight(.semibold)
            .padding(.horizontal, 8)
            .padding(.vertical, 2)
            .background(Capsule().fill(slot.effectiveTint.opacity(0.18)))
            .foregroundStyle(slot.effectiveTint)
    }
}

struct InfoTab: View {
    let slot: Runny_V1_SlotStatus

    var body: some View {
        Form {
            LabeledContent("Slot", value: slot.slot)
            if !slot.runnerName.isEmpty {
                LabeledContent("Runner name", value: slot.runnerName)
            }
            LabeledContent("State", value: SlotPresentation.stateLabel(slot))
            LabeledContent("Entered", value: Self.timestamp.string(from: slot.stateEntered.dateValue))
            if !slot.cycleID.isEmpty {
                LabeledContent("Cycle", value: slot.cycleID)
            }
            if slot.hasVm {
                if !slot.vm.ip.isEmpty { LabeledContent("IP", value: slot.vm.ip) }
                if !slot.vm.mac.isEmpty { LabeledContent("MAC", value: slot.vm.mac) }
            }
            if slot.hasJob {
                LabeledContent("Job", value: slot.job.name)
                LabeledContent("Job started", value: Self.timestamp.string(from: slot.job.started.dateValue))
            }
            if slot.consecutiveFailures > 0 {
                LabeledContent("Consecutive failures", value: "\(slot.consecutiveFailures)")
            }
            if slot.state == .backoff, slot.backoffSeconds > 0 {
                LabeledContent("Backoff", value: "\(slot.backoffSeconds)s")
            }
            if !slot.lastFailure.isEmpty {
                LabeledContent("Last failure", value: slot.lastFailure)
            }
            if !slot.detail.isEmpty {
                LabeledContent("Detail", value: slot.detail)
            }
            LabeledContent("Paused", value: slot.paused ? "yes" : "no")
            if slot.wedged {
                LabeledContent("Wedged", value: "yes — parked until the daemon restarts")
            }
        }
        .formStyle(.grouped)
        .textSelection(.enabled)
    }

    static let timestamp: DateFormatter = {
        let formatter = DateFormatter()
        formatter.dateStyle = .medium
        formatter.timeStyle = .medium
        return formatter
    }()
}
