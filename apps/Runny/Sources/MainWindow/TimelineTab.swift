import RunnyV1
import SwiftUI

/// Completed-cycle timeline, Why-backed. Fixed-step rows in cycle order —
/// never a proportional bar: LISTENING runs for hours while CLONE takes a
/// second, so proportional rendering makes 10 of 12 states sub-pixel and
/// hides exactly the short state a failure usually lives in. The per-row
/// bar is log-scaled and capped, a visual hint only; the duration text is
/// the datum.
struct TimelineTab: View {
    @Environment(DaemonStore.self) private var store

    let slot: Runny_V1_SlotStatus
    @State private var model = CycleHistoryModel()
    @State private var selectedCycle: String?

    var body: some View {
        VStack(spacing: 0) {
            picker
                .padding(10)
            Divider()
            content
        }
        .onAppear { model.refreshIfNeeded(slot: slot, store: store) }
        .onChange(of: slot.cycleID) {
            model.refreshIfNeeded(slot: slot, store: store)
        }
    }

    private var picker: some View {
        HStack {
            Picker("Completed cycle", selection: $selectedCycle) {
                ForEach(model.cycles, id: \.cycleID) { cycle in
                    Text(pickerLabel(cycle)).tag(Optional(cycle.cycleID))
                }
            }
            .disabled(model.cycles.isEmpty)
            Spacer()
            if model.loading { ProgressView().controlSize(.small) }
            Button {
                model.refresh(slotName: slot.slot, cycleID: slot.cycleID, store: store)
            } label: {
                Image(systemName: "arrow.clockwise")
            }
            .help("Reload cycle history")
        }
    }

    @ViewBuilder
    private var content: some View {
        if let error = model.loadError {
            ContentUnavailableView(
                "Couldn't load cycles", systemImage: "exclamationmark.triangle",
                description: Text(error)
            )
        } else if model.cycles.isEmpty {
            ContentUnavailableView(
                "No completed cycles", systemImage: "clock.arrow.circlepath",
                description: Text("The timeline shows finished cycles; the current cycle appears here once it completes.")
            )
        } else if let cycle = selectedOrNewest {
            CycleView(cycle: cycle)
        }
    }

    private var selectedOrNewest: Runny_V1_CycleRecord? {
        if let selectedCycle,
           let match = model.cycles.first(where: { $0.cycleID == selectedCycle })
        {
            return match
        }
        return model.cycles.first // newest-first from the server
    }

    private func pickerLabel(_ cycle: Runny_V1_CycleRecord) -> String {
        let started = Self.started.string(from: cycle.started.dateValue)
        let mark = cycle.result == "success" ? "✓" : "✗"
        return "\(mark) \(cycle.cycleID) · \(started)"
    }

    static let started: DateFormatter = {
        let formatter = DateFormatter()
        formatter.dateStyle = .short
        formatter.timeStyle = .short
        return formatter
    }()
}

struct CycleView: View {
    let cycle: Runny_V1_CycleRecord

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 12) {
                summary
                Divider()
                states
                if !cycle.injectedKeys.isEmpty {
                    Divider()
                    injectedKeys
                }
                if !cycle.artifacts.isEmpty {
                    Divider()
                    artifacts
                }
            }
            .padding()
        }
        .textSelection(.enabled)
    }

    private var summary: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack(spacing: 6) {
                if cycle.result == "success" {
                    Label("success", systemImage: "checkmark.circle.fill")
                        .foregroundStyle(.green)
                } else {
                    Label(
                        "failed in \(failureStateName): \(cycle.failureError)",
                        systemImage: "xmark.circle.fill"
                    )
                    .foregroundStyle(.red)
                    .lineLimit(3)
                }
                Spacer()
                Text("total \(SlotPresentation.duration(cycle.finished.dateValue.timeIntervalSince(cycle.started.dateValue)))")
                    .font(.callout)
                    .monospacedDigit()
                    .foregroundStyle(.secondary)
            }
            .font(.callout)
            HStack(spacing: 12) {
                // Configured ref (intent) and resolved digest (truth) — the
                // pair `runnyctl why` renders.
                if !cycle.image.isEmpty {
                    Text(cycle.image)
                        .lineLimit(1)
                        .truncationMode(.middle)
                }
                if !cycle.imageDigest.isEmpty {
                    Text(String(cycle.imageDigest.prefix(19)) + "…")
                }
                if !cycle.runnerVersion.isEmpty {
                    Text(cycle.runnerVersion)
                        .lineLimit(1)
                        .truncationMode(.middle)
                }
                if cycle.hasVm, !cycle.vm.ip.isEmpty {
                    Text("vm \(cycle.vm.ip)")
                }
                if cycle.hasJob {
                    Text("job \"\(cycle.job.name)\"")
                        .lineLimit(1)
                        .truncationMode(.tail)
                }
            }
            .font(.caption)
            .monospacedDigit()
            .foregroundStyle(.secondary)
        }
    }

    private var failureStateName: String {
        cycle.failureState.isEmpty ? "?" : cycle.failureState
    }

    private var states: some View {
        // Sort by cycle order, not enum order; a cycle's record only holds
        // states actually entered (no SECURE_SSH when hardening is off,
        // truncated on early failure) — absent states simply don't render.
        let ordered = cycle.states.sorted {
            $0.state.cycleIndex < $1.state.cycleIndex
        }
        return VStack(alignment: .leading, spacing: 0) {
            ForEach(Array(ordered.enumerated()), id: \.offset) { _, record in
                StateRow(record: record)
            }
        }
    }

    private var artifacts: some View {
        VStack(alignment: .leading, spacing: 2) {
            Text("Artifacts")
                .font(.caption)
                .fontWeight(.semibold)
                .foregroundStyle(.secondary)
            ForEach(cycle.artifacts, id: \.self) { artifact in
                Label(artifact, systemImage: "doc.text")
                    .font(.caption)
                    .monospaced()
                    .help("Retained in this cycle's directory under the runny home")
            }
        }
    }

    /// Operator debug-key audit trail: every attempt against this cycle's
    /// guest, including refused and mid-job ones. A "JOB"-state entry is a
    /// contamination event — the job ran with an operator credential present.
    private var injectedKeys: some View {
        VStack(alignment: .leading, spacing: 4) {
            Text("Debug keys")
                .font(.caption)
                .fontWeight(.semibold)
                .foregroundStyle(.secondary)
            ForEach(Array(cycle.injectedKeys.enumerated()), id: \.offset) { _, key in
                HStack(alignment: .firstTextBaseline, spacing: 6) {
                    Image(systemName: key.state == "JOB" ? "exclamationmark.shield" : "key")
                        .foregroundStyle(key.state == "JOB" ? .orange : .secondary)
                    VStack(alignment: .leading, spacing: 1) {
                        Text("\(key.fingerprint) — \(key.outcome) in \(key.state)")
                            .font(.caption)
                            .monospaced()
                        if !key.reason.isEmpty || !key.error.isEmpty {
                            Text([key.reason, key.error].filter { !$0.isEmpty }.joined(separator: " · "))
                                .font(.caption2)
                                .foregroundStyle(.secondary)
                        }
                    }
                }
            }
        }
    }
}

struct StateRow: View {
    let record: Runny_V1_StateRecord

    private var seconds: TimeInterval {
        record.left.dateValue.timeIntervalSince(record.entered.dateValue)
    }

    /// Log-scaled width hint, capped: 1s ≈ 40pt, an hour ≈ 180pt.
    private var barWidth: CGFloat {
        let s = max(seconds, 0.05)
        return CGFloat(min(40 + 38 * log10(s + 1) * 2.4, 180))
    }

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            Text(record.state.displayName)
                .font(.callout)
                .monospaced()
                .frame(width: 120, alignment: .leading)
            RoundedRectangle(cornerRadius: 2)
                .fill(barColor.opacity(0.45))
                .frame(width: barWidth, height: 8)
            Text(SlotPresentation.duration(seconds))
                .font(.callout)
                .monospacedDigit()
                .foregroundStyle(.secondary)
                .frame(width: 64, alignment: .trailing)
            outcome
            Spacer(minLength: 0)
        }
        .padding(.vertical, 3)
    }

    @ViewBuilder
    private var outcome: some View {
        switch record.outcome {
        case "ok":
            Image(systemName: "checkmark")
                .foregroundStyle(.green)
                .font(.caption)
        case "deadline":
            Label("DEADLINE: \(record.error)", systemImage: "clock.badge.exclamationmark")
                .foregroundStyle(.red)
                .font(.caption)
                .lineLimit(2)
        case "error":
            Label(record.error, systemImage: "xmark")
                .foregroundStyle(.red)
                .font(.caption)
                .lineLimit(2)
        default:
            EmptyView()
        }
    }

    private var barColor: Color {
        record.outcome == "ok" ? record.state.tint : .red
    }
}
