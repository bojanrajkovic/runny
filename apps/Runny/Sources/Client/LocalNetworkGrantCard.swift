import AppKit
import RunnyV1
import SwiftUI

/// The proactive Local Network (TCC) grant surface. Driven by the daemon-published
/// `local_network_grant` signal, NEVER the manual `doctorChecks` (which is nil
/// until Run Checks and reports ok until a guest boots — it cannot fire *before*
/// the first guest dial fails). The point is to surface the grant ask before a
/// cycle dies at AWAIT_SSH with "no route to host".
enum LocalNetworkCard: Equatable {
    /// Reachable, an old daemon (UNSPECIFIED), or no live connection — show nothing.
    case hidden
    /// No vmnet interface yet, so reachability is undetermined — the Local Network
    /// prompt may be pending. Surfaced as a soft "grant it if guests are
    /// unreachable", proactively, rather than waiting for a dial to fail.
    case pendingOrUnknown
    /// A vmnet interface is up but the subnet is unreachable — the TCC denial is
    /// confirmed. Surfaced firmly.
    case denied
}

extension DaemonStore {
    /// Map the daemon's grant signal to a card. Pure → unit-tested. UNKNOWN is the
    /// "prompt may be pending" case the naive doctor verdict would miss (it reports
    /// ok with no vmnet); REACHABLE and UNSPECIFIED show nothing.
    nonisolated static func localNetworkVerdict(grant: Runny_V1_LocalNetworkGrant) -> LocalNetworkCard {
        switch grant {
        case .unknown: .pendingOrUnknown
        case .denied: .denied
        case .reachable, .unspecified: .hidden
        @unknown default: .hidden
        }
    }

    /// The card to show, gated on a live connection: a stale grant from a dropped
    /// daemon must not linger (the Start affordance owns "daemon down"). Mirrors
    /// `visibleSkew`'s connection gate.
    var localNetworkCard: LocalNetworkCard {
        guard case .connected = connection else { return .hidden }
        return Self.localNetworkVerdict(grant: localNetworkGrant)
    }
}

/// The grant card for the menu bar and main window. Self-hides unless the daemon
/// reports a non-reachable grant; renders a System Settings deep link to the
/// Local Network pane. Placed unconditionally by callers.
struct LocalNetworkGrantCard: View {
    @Environment(DaemonStore.self) private var store

    /// The Local Network privacy pane. macOS-version-fragile (tracked as a
    /// follow-up); a stale anchor opens Privacy & Security at the top rather than
    /// failing, so the CTA degrades to "we opened Settings", never a dead button.
    private static let settingsURL = URL(
        string: "x-apple.systempreferences:com.apple.preference.security?Privacy_LocalNetwork"
    )

    var body: some View {
        switch store.localNetworkCard {
        case .hidden:
            EmptyView()
        case .pendingOrUnknown:
            card(
                "Runny may need Local Network access to reach guests. If runners can't be reached, grant it.",
                tint: .orange
            )
        case .denied:
            card(
                "Local Network access is denied, so runnyd can't reach guests. Grant it in System Settings.",
                tint: .red
            )
        }
    }

    private func card(_ text: String, tint: Color) -> some View {
        AffordanceRow(systemImage: "network.badge.shield.half.filled", text: text, tint: tint) {
            Button("Open Settings") {
                if let url = Self.settingsURL { NSWorkspace.shared.open(url) }
            }
            .controlSize(.small)
        }
    }
}
