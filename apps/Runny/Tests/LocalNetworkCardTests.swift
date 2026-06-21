import RunnyV1
import XCTest

@testable import Runny

/// The Local Network grant card verdict, driven by the daemon's tri-state signal.
/// The load-bearing case is UNKNOWN → pendingOrUnknown: that is the "no vmnet yet,
/// prompt may be pending" window the naive doctor verdict (ok until a guest boots)
/// would silently miss. Pure → no live daemon.
final class LocalNetworkCardTests: XCTestCase {
    func testDeniedShowsTheCard() {
        XCTAssertEqual(DaemonStore.localNetworkVerdict(grant: .denied, isSystemDaemon: false), .denied)
    }

    func testUnknownIsPromptMayBePendingNotHidden() {
        // The case a doctorChecks-driven verdict would miss: no vmnet interface yet
        // reads as "prompt may be pending", surfaced proactively — never hidden.
        XCTAssertEqual(DaemonStore.localNetworkVerdict(grant: .unknown, isSystemDaemon: false), .pendingOrUnknown)
    }

    func testReachableAndUnspecifiedHideTheCard() {
        XCTAssertEqual(DaemonStore.localNetworkVerdict(grant: .reachable, isSystemDaemon: false), .hidden)
        // UNSPECIFIED = old daemon / not sampled — no card, defer to the skew banner.
        XCTAssertEqual(DaemonStore.localNetworkVerdict(grant: .unspecified, isSystemDaemon: false), .hidden)
    }

    func testSystemDaemonNeverShowsTheCard() {
        // A launchd-started system daemon is auto-allowed Local Network (TN3179), so
        // the card must NEVER show for it — including on the UNKNOWN/DENIED grants that
        // surface it for a per-user agent.
        for grant: Runny_V1_LocalNetworkGrant in [.unknown, .denied, .reachable, .unspecified] {
            XCTAssertEqual(
                DaemonStore.localNetworkVerdict(grant: grant, isSystemDaemon: true), .hidden,
                "grant \(grant) must be hidden for a system daemon"
            )
        }
    }
}
