import Foundation
import RunnyV1
import SwiftProtobuf

extension Google_Protobuf_Timestamp {
    /// Explicit conversion; avoids depending on SwiftProtobuf convenience
    /// initializers that have shifted across versions.
    var dateValue: Date {
        Date(timeIntervalSince1970: TimeInterval(seconds) + TimeInterval(nanos) / 1_000_000_000)
    }
}

/// Pure presentation logic shared by the popover and the main window.
/// Mirrors runnyctl's status rendering semantics exactly (cmd/runnyctl);
/// the two clients are siblings and should describe a slot the same way.
enum SlotPresentation {
    /// Duration formatting matching runnyctl's durString: seconds-rounded
    /// under an hour, minutes-rounded at an hour and above, negatives clamp
    /// to 0s.
    static func duration(_ interval: TimeInterval) -> String {
        let clamped = max(interval, 0)
        if clamped < 3600 {
            let secs = Int(clamped.rounded())
            if secs < 60 { return "\(secs)s" }
            return "\(secs / 60)m\(secs % 60 == 0 ? "" : "\(secs % 60)s")"
        }
        let mins = Int((clamped / 60).rounded())
        return "\(mins / 60)h\(mins % 60 == 0 ? "" : "\(mins % 60)m")"
    }

    /// The STATE label: name, with runnyctl's paused/wedged treatment —
    /// "*" suffix while paused, replaced entirely by "WEDGED!" when wedged.
    static func stateLabel(_ slot: Runny_V1_SlotStatus) -> String {
        if slot.wedged { return "WEDGED!" }
        return slot.state.displayName + (slot.paused ? "*" : "")
    }

    /// Display name: the GitHub-visible runner name, falling back to the
    /// bare slot name when no runner exists (BACKOFF).
    static func displayName(_ slot: Runny_V1_SlotStatus) -> String {
        slot.runnerName.isEmpty ? slot.slot : slot.runnerName
    }

    /// Seconds until the next retry while in BACKOFF, nil otherwise or once
    /// elapsed. Computed client-side from the local clock, like runnyctl.
    static func retryIn(_ slot: Runny_V1_SlotStatus, now: Date) -> TimeInterval? {
        guard slot.state == .backoff, slot.backoffSeconds > 0 else { return nil }
        let remaining = TimeInterval(slot.backoffSeconds) - now.timeIntervalSince(slot.stateEntered.dateValue)
        return remaining > 0 ? remaining : nil
    }

    /// The NOTE chain, runnyctl's exact priority: base is last_failure,
    /// prefixed with the consecutive-failure count when nonzero; a live
    /// `detail` annotation overrides all of that; and in BACKOFF the retry
    /// countdown is PREPENDED — it is the useful number there, never hidden
    /// behind stale failure text.
    static func note(_ slot: Runny_V1_SlotStatus, now: Date) -> String {
        var note = slot.lastFailure
        if slot.consecutiveFailures > 0 {
            let n = slot.consecutiveFailures
            let prefix = "\(n) consecutive failure\(n == 1 ? "" : "s")"
            note = note.isEmpty ? prefix : "\(prefix); \(note)"
        }
        if !slot.detail.isEmpty {
            note = slot.detail
        }
        if let remaining = retryIn(slot, now: now) {
            let retry = "retry in \(duration(remaining))"
            note = note.isEmpty ? retry : "\(retry); \(note)"
        }
        return note
    }

    /// Time spent in the current state, clamped at zero.
    static func timeInState(_ slot: Runny_V1_SlotStatus, now: Date) -> TimeInterval {
        max(now.timeIntervalSince(slot.stateEntered.dateValue), 0)
    }
}
