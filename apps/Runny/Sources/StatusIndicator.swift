import RunnyV1
import SwiftUI

/// The slot-health dot, made accessible: color still reads at a glance, but
/// the shape differs for the states that matter most without it (wedged is a
/// warning triangle, paused a pause glyph), and every instance carries a
/// VoiceOver label. Color alone never carries the meaning (a11y).
struct StatusIndicator: View {
    let slot: Runny_V1_SlotStatus
    var size: CGFloat = 8

    var body: some View {
        Image(systemName: symbol)
            .font(.system(size: size))
            .foregroundStyle(slot.effectiveTint)
            .accessibilityLabel(accessibilityText)
    }

    private var symbol: String {
        if slot.wedged { return "exclamationmark.triangle.fill" }
        if slot.paused { return "pause.circle.fill" }
        return "circle.fill"
    }

    private var accessibilityText: String {
        var text = SlotPresentation.statePhrase(slot)
        if slot.paused, !slot.wedged { text += ", paused" }
        return text
    }
}
