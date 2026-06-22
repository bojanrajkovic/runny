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
    /// confirmed. Surfaced firmly, with the System Settings grant CTA.
    case denied
    /// runnyd was self-daemonized / reparented — launchd did not start it — so
    /// macOS denies it guest access. Distinct from `denied`: the System Settings
    /// grant cannot repair the launch context, so this surfaces the launch-context
    /// remediation (start via launchd or run foreground) with NO Settings button.
    case selfDaemonized
}

extension DaemonStore {
    /// Map the daemon's grant signal to a card. Pure → unit-tested. UNKNOWN is the
    /// "prompt may be pending" case the naive doctor verdict would miss (it reports
    /// ok with no vmnet); REACHABLE and UNSPECIFIED show nothing.
    nonisolated static func localNetworkVerdict(
        grant: Runny_V1_LocalNetworkGrant, isSystemDaemon: Bool
    ) -> LocalNetworkCard {
        // A launchd-started system daemon is auto-allowed Local Network regardless of
        // uid (Apple TN3179), so its grant can never be pending or denied — never
        // surface the card, whose "open System Settings" CTA is a per-user-agent
        // affordance and a structural dead end here.
        if isSystemDaemon { return .hidden }
        switch grant {
        case .unknown: return .pendingOrUnknown
        case .denied: return .denied
        case .selfDaemonized: return .selfDaemonized
        case .reachable, .unspecified: return .hidden
        @unknown default: return .hidden
        }
    }

    /// The card to show, gated on a live connection: a stale grant from a dropped
    /// daemon must not linger (the Start affordance owns "daemon down"). Mirrors
    /// `visibleSkew`'s connection gate. Suppressed entirely for a system daemon
    /// (auto-allowed Local Network — the card structurally cannot be true there).
    var localNetworkCard: LocalNetworkCard {
        guard case .connected = connection else { return .hidden }
        return Self.localNetworkVerdict(grant: localNetworkGrant, isSystemDaemon: RunnyHome.resolvesToSystemHome)
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
            settingsCard(
                "Runny may need Local Network access to reach guests. If runners can't be reached, grant it.",
                tint: .orange
            )
        case .denied:
            settingsCard(
                "Local Network access is denied, so runnyd can't reach guests. Grant it in System Settings.",
                tint: .red
            )
        case .selfDaemonized:
            // No trailing button: the TCC grant can't fix a daemon launchd didn't
            // start. The remediation is the launch context, so the card only explains.
            card(
                "runnyd was started in the background, so macOS denies it access to guests. "
                    + "Toggling Local Network access won't help — start it via launchd or run it in the "
                    + "foreground, never background it.",
                tint: .red
            )
        }
    }

    /// One AffordanceRow per grant state, with the shared icon defined once. The
    /// trailing control defaults to none (the self-daemonized card, whose fix is
    /// the launch context, not a Settings deep link); the grantable states pass an
    /// "Open Settings" button.
    private func card(
        _ text: String, tint: Color, @ViewBuilder trailing: () -> some View = { EmptyView() }
    ) -> some View {
        AffordanceRow(
            systemImage: "network.badge.shield.half.filled", text: text, tint: tint, trailing: trailing
        )
    }

    private func settingsCard(_ text: String, tint: Color) -> some View {
        card(text, tint: tint) {
            Button("Open Settings") {
                if let url = Self.settingsURL { NSWorkspace.shared.open(url) }
            }
            .controlSize(.small)
        }
    }
}
