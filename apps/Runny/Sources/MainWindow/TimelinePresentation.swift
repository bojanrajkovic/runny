import Foundation
import RunnyV1

/// Pure presentation logic for TimelineTab/CurrentCycleView, pulled out of
/// the view bodies so it's unit-testable without SwiftUI — mirrors
/// SlotPresentation's split for the slot list.
enum TimelinePresentation {
    /// The slot is actively running a cycle (not parked in BACKOFF between
    /// attempts, and not wedged): only then is there a "current cycle" to
    /// show. A wedged slot keeps a non-empty cycleID and a TEARDOWN state
    /// after its cycle record is already finished — showing it as a live
    /// cycle would tick a forever-growing TEARDOWN that duplicates the
    /// completed failure record.
    static func hasCurrent(_ slot: Runny_V1_SlotStatus) -> Bool {
        !slot.cycleID.isEmpty && slot.state != .backoff
            && slot.state != .unspecified && !slot.wedged
    }

    /// Whether `selection` still points at something the timeline can show:
    /// `.current` only while a cycle is actually running, `.cycle(id)` only
    /// while that id is still in the loaded history.
    static func isValid(_ selection: TimelineTab.Selection, hasCurrent: Bool, cycleIDs: [String]) -> Bool {
        switch selection {
        case .current: hasCurrent
        case let .cycle(id): cycleIDs.contains(id)
        }
    }

    // Deliberately still a DateFormatter, not Date.FormatStyle: FormatStyle's
    // `.numeric` renders a 4-digit year where `.short/.short` renders
    // 2-digit — don't "finish" this migration, it would change the picker
    // label's format.
    static let pickerDateFormatter: DateFormatter = {
        let formatter = DateFormatter()
        formatter.dateStyle = .short
        formatter.timeStyle = .short
        return formatter
    }()

    /// "✓ cycleID · started" — the cycle picker's row label.
    static func pickerLabel(_ cycle: Runny_V1_CycleRecord) -> String {
        let started = pickerDateFormatter.string(from: cycle.started.dateValue)
        return "\(CycleVerdict(cycle).mark) \(cycle.cycleID) · \(started)"
    }

    /// Completed-state durations keyed by SlotState for O(1) lookup per row.
    /// Negative/zero-or-less elapsed (a clock anomaly, or a state entered and
    /// left at the same instant) is dropped rather than shown as a bogus
    /// duration.
    static func completedDurations(for slot: Runny_V1_SlotStatus) -> [Runny_V1_SlotState: TimeInterval] {
        var d: [Runny_V1_SlotState: TimeInterval] = [:]
        for record in slot.activeCycleStates {
            let elapsed = record.left.dateValue.timeIntervalSince(record.entered.dateValue)
            if elapsed >= 0 {
                d[record.state] = elapsed
            }
        }
        return d
    }

    /// The forward path, BACKOFF excluded (it's the between-cycles park, not
    /// a step). DEBUG only appears once it's armed or active — most cycles
    /// never take it. Other optional states (e.g. SECURE_SSH when hardening
    /// is off) simply show as not-yet-reached; the live clock is on the
    /// current state.
    static func pipelinePath(for slot: Runny_V1_SlotStatus) -> [Runny_V1_SlotState] {
        Runny_V1_SlotState.cycleOrder.filter { state in
            switch state {
            case .backoff: false
            case .debug: slot.debugHoldArmed || slot.state == .debug
            default: true
            }
        }
    }

    /// Where `state` sits relative to the slot's current pipeline position.
    static func pipelinePosition(of state: Runny_V1_SlotState, currentIndex: Int) -> PipelineRow.Position {
        if state.cycleIndex < currentIndex { return .passed }
        if state.cycleIndex == currentIndex { return .current }
        return .pending
    }
}
