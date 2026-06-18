import SwiftUI

/// The shared banner row for the lifecycle affordances (daemon-not-running Start,
/// post-upgrade update, Local Network grant): a caption icon + text + a trailing
/// control, tinted, with a `.secondary` tint rendering background-less. One
/// definition so the three surfaces — placed adjacent in the menu bar and main
/// window — cannot drift in spacing or padding. `StatusBanner` is the
/// dismiss-button sibling for the drain/error vocabulary; this is the
/// action-slot one.
struct AffordanceRow<Trailing: View>: View {
    let systemImage: String
    let text: String
    let tint: Color
    @ViewBuilder let trailing: Trailing

    var body: some View {
        HStack(spacing: 6) {
            Image(systemName: systemImage)
                .font(.caption)
                .foregroundStyle(tint)
            Text(text)
                .font(.caption)
                .foregroundStyle(.primary)
                .fixedSize(horizontal: false, vertical: true)
            Spacer(minLength: 4)
            trailing
        }
        .padding(.horizontal, Metrics.pad)
        .padding(.vertical, 6)
        .background(tint == .secondary ? Color.clear : tint.opacity(0.08))
    }
}
