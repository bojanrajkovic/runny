import AppKit
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
    @State private var selection: Selection?

    /// Either the live, in-flight cycle or a completed one. The current cycle
    /// has no cycle.json yet, so it can't be a `CycleRecord` — it's its own
    /// case, rendered from the live snapshot.
    enum Selection: Hashable {
        case current
        case cycle(String)
    }

    /// The slot is actively running a cycle (not parked in BACKOFF between
    /// attempts, and not wedged): only then is there a "current cycle" to show.
    /// A wedged slot keeps a non-empty cycleID and a TEARDOWN state after its
    /// cycle record is already finished — showing it as a live cycle would tick
    /// a forever-growing TEARDOWN that duplicates the completed failure record.
    private var hasCurrent: Bool {
        !slot.cycleID.isEmpty && slot.state != .backoff
            && slot.state != .unspecified && !slot.wedged
    }

    var body: some View {
        VStack(spacing: 0) {
            picker
                .padding(10)
            Divider()
            content
        }
        .onAppear {
            model.refreshIfNeeded(slot: slot, store: store)
            normalizeSelection()
        }
        .onChange(of: slot.cycleID) {
            model.refreshIfNeeded(slot: slot, store: store)
            normalizeSelection()
        }
        // If the pane opened while the daemon was unreachable, the load failed
        // and neither hook above fires on reconnect — retry once the client is
        // back instead of stranding it on "daemon unreachable" until Reload.
        .onChange(of: store.client == nil) {
            if store.client != nil {
                model.refreshIfNeeded(slot: slot, store: store)
            }
        }
        // Cycles arrive asynchronously; pick a default once they land.
        .onChange(of: model.cycles.map(\.cycleID)) { normalizeSelection() }
    }

    /// Default to the live cycle when one is running, else the most recent
    /// completed cycle — never blank. Leaves a valid existing pick alone, so
    /// a cycle advancing doesn't yank the user off a past cycle they opened.
    private func normalizeSelection() {
        if let selection, isValid(selection) { return }
        if hasCurrent {
            selection = .current
        } else if let newest = model.cycles.first {
            selection = .cycle(newest.cycleID)
        } else {
            selection = nil
        }
    }

    private func isValid(_ selection: Selection) -> Bool {
        switch selection {
        case .current: hasCurrent
        case let .cycle(id): model.cycles.contains { $0.cycleID == id }
        }
    }

    private var picker: some View {
        HStack {
            Picker("Cycle", selection: $selection) {
                if hasCurrent {
                    Text("● Current cycle").tag(Optional(Selection.current))
                }
                ForEach(model.cycles, id: \.cycleID) { cycle in
                    Text(pickerLabel(cycle)).tag(Optional(Selection.cycle(cycle.cycleID)))
                }
            }
            .disabled(!hasCurrent && model.cycles.isEmpty)
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
        switch selection {
        case .current:
            CurrentCycleView(slot: slot)
        case let .cycle(id):
            if let cycle = model.cycles.first(where: { $0.cycleID == id }) {
                CycleView(cycle: cycle)
            } else {
                placeholder
            }
        case .none:
            placeholder
        }
    }

    @ViewBuilder
    private var placeholder: some View {
        if let error = model.loadError {
            ContentUnavailableView(
                "Couldn't load cycles", systemImage: "exclamationmark.triangle",
                description: Text(error)
            )
        } else if !model.loading {
            ContentUnavailableView(
                "No completed cycles", systemImage: "clock.arrow.circlepath",
                description: Text("Finished cycles show here; the current cycle is selectable above while it runs.")
            )
        }
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

/// The in-flight cycle: position in the pipeline with live per-state durations
/// for completed states. Durations come from `slot.activeCycleStates`, populated
/// by the daemon as each state finishes. Older daemons send an empty list;
/// the view degrades gracefully (completed rows show no duration).
struct CurrentCycleView: View {
    let slot: Runny_V1_SlotStatus

    /// Completed-state durations keyed by SlotState for O(1) lookup per row.
    private var completedDurations: [Runny_V1_SlotState: TimeInterval] {
        var d: [Runny_V1_SlotState: TimeInterval] = [:]
        for record in slot.activeCycleStates {
            let elapsed = record.left.dateValue.timeIntervalSince(record.entered.dateValue)
            if elapsed >= 0 {
                d[record.state] = elapsed
            }
        }
        return d
    }

    /// The forward path, BACKOFF excluded (it's the between-cycles park, not a
    /// step). DEBUG only appears once it's armed or active — most cycles never
    /// take it. Other optional states (e.g. SECURE_SSH when hardening is off)
    /// simply show as not-yet-reached; the live clock is on the current state.
    private var path: [Runny_V1_SlotState] {
        Runny_V1_SlotState.cycleOrder.filter { state in
            switch state {
            case .backoff: false
            case .debug: slot.debugHoldArmed || slot.state == .debug
            default: true
            }
        }
    }

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 12) {
                // No metadata header here: the detail header above and the Info
                // tab already carry the image, digest, runner, vm, and live
                // state. The current cycle's unique datum is where it is now.
                pipeline
            }
            .padding()
        }
        .textSelection(.enabled)
    }

    private var pipeline: some View {
        let currentIndex = slot.state.cycleIndex
        let durations = completedDurations
        return VStack(alignment: .leading, spacing: 0) {
            ForEach(path, id: \.self) { state in
                PipelineRow(
                    state: state,
                    position: position(of: state, currentIndex: currentIndex),
                    slot: slot,
                    completedDuration: durations[state]
                )
            }
        }
    }

    private func position(of state: Runny_V1_SlotState, currentIndex: Int) -> PipelineRow.Position {
        if state.cycleIndex < currentIndex { return .passed }
        if state.cycleIndex == currentIndex { return .current }
        return .pending
    }
}

/// One pipeline step: a position glyph (passed / you-are-here / pending), the
/// FSM token (matching the completed-cycle timeline's vocabulary), and a
/// duration: live clock on the current step, recorded duration on passed steps
/// (nil when the daemon is older and doesn't stream active-cycle state history).
struct PipelineRow: View {
    enum Position { case passed, current, pending }

    let state: Runny_V1_SlotState
    let position: Position
    let slot: Runny_V1_SlotStatus
    /// Non-nil for passed states when the daemon is streaming active-cycle
    /// history (`SlotStatus.active_cycle_states`). Nil on older daemons.
    var completedDuration: TimeInterval?

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            Image(systemName: glyph)
                .font(.caption)
                .foregroundStyle(glyphTint)
                .accessibilityLabel(accessibilityLabel)
            Text(state.displayName)
                .font(.callout)
                .monospaced()
                .fontWeight(position == .current ? .semibold : .regular)
                .foregroundStyle(position == .pending ? .secondary : .primary)
                .frame(width: 120, alignment: .leading)
            switch position {
            case .current:
                TickingText { now in
                    "for \(SlotPresentation.duration(SlotPresentation.timeInState(slot, now: now)))"
                }
                .font(.callout)
                .monospacedDigit()
                .foregroundStyle(.secondary)
            case .passed:
                if let d = completedDuration {
                    Text(SlotPresentation.duration(d))
                        .font(.callout)
                        .monospacedDigit()
                        .foregroundStyle(.secondary)
                }
            case .pending:
                EmptyView()
            }
            Spacer(minLength: 0)
        }
        .padding(.vertical, 5)
        .padding(.horizontal, 8)
        .background {
            if position == .current {
                RoundedRectangle(cornerRadius: 6).fill(state.tint.opacity(0.12))
            }
        }
    }

    private var glyph: String {
        switch position {
        case .passed: "circle.fill"
        case .current: "largecircle.fill.circle"
        case .pending: "circle"
        }
    }

    private var glyphTint: Color {
        switch position {
        case .passed: .secondary
        case .current: state.tint
        case .pending: .secondary.opacity(0.45)
        }
    }

    private var accessibilityLabel: String {
        switch position {
        case .passed: "passed"
        case .current: "current"
        case .pending: "pending"
        }
    }
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
                ArtifactRow(cycle: cycle, filename: artifact)
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

/// One retained artifact, made actionable. The app and daemon share a host
/// (unix socket), so the file is local: click reveals it in Finder, the menu
/// opens it or copies the path. The on-disk path is reconstructed from the
/// cycle record (it carries only the filename), guarded by an existence check
/// so a daemon layout drift surfaces an error instead of doing nothing.
struct ArtifactRow: View {
    @Environment(DaemonStore.self) private var store
    let cycle: Runny_V1_CycleRecord
    let filename: String

    var body: some View {
        Button(action: reveal) {
            Label(filename, systemImage: "doc.text")
                .font(.caption)
                .monospaced()
        }
        .buttonStyle(.plain)
        .contextMenu {
            Button("Reveal in Finder", action: reveal)
            Button("Open", action: open)
            Button("Copy Path") { Pasteboard.copy(url.path) }
        }
        .help("Reveal this cycle's artifact in Finder")
    }

    private var url: URL {
        if !cycle.cycleDir.isEmpty {
            return URL(fileURLWithPath: cycle.cycleDir).appendingPathComponent(filename)
        }
        return RunnyHome.artifactURL(cycle: cycle, filename: filename)
    }

    private func reveal() {
        guard ensurePresent() else { return }
        NSWorkspace.shared.activateFileViewerSelecting([url])
    }

    private func open() {
        guard ensurePresent() else { return }
        NSWorkspace.shared.open(url)
    }

    private func ensurePresent() -> Bool {
        guard FileManager.default.fileExists(atPath: url.path) else {
            store.commandError =
                "couldn't find \(filename) on disk — looked in \(url.deletingLastPathComponent().path)"
            return false
        }
        return true
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
        case "warn":
            // The state did its mandatory job; a best-effort cleanup left an
            // orphan. Non-fatal — orange, not red, and the detail stays visible.
            Label(record.error, systemImage: "exclamationmark.triangle")
                .foregroundStyle(.orange)
                .font(.caption)
                .lineLimit(2)
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
        switch record.outcome {
        case "ok": record.state.tint
        case "warn": .orange
        default: .red
        }
    }
}
