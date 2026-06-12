import SwiftUI

/// The one leaf that ticks. Every live duration/age label goes through this
/// so the tick-or-freeze choice is made once: TimelineView at the leaf Text
/// only (it pauses off-screen), 1s cadence, monospaced digits.
struct TickingText: View {
    let make: (Date) -> String

    var body: some View {
        TimelineView(.periodic(from: .now, by: 1)) { context in
            Text(make(context.date))
                .monospacedDigit()
        }
    }
}
