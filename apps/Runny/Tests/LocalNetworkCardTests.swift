import RunnyV1
import XCTest

@testable import Runny

/// The Local Network grant card verdict, driven by the daemon's tri-state signal.
/// The load-bearing case is UNKNOWN → pendingOrUnknown: that is the "no vmnet yet,
/// prompt may be pending" window the naive doctor verdict (ok until a guest boots)
/// would silently miss. Pure → no live daemon.
final class LocalNetworkCardTests: XCTestCase {
    func testDeniedShowsTheCard() {
        XCTAssertEqual(DaemonStore.localNetworkVerdict(grant: .denied), .denied)
    }

    func testUnknownIsPromptMayBePendingNotHidden() {
        // The case a doctorChecks-driven verdict would miss: no vmnet interface yet
        // reads as "prompt may be pending", surfaced proactively — never hidden.
        XCTAssertEqual(DaemonStore.localNetworkVerdict(grant: .unknown), .pendingOrUnknown)
    }

    func testReachableAndUnspecifiedHideTheCard() {
        XCTAssertEqual(DaemonStore.localNetworkVerdict(grant: .reachable), .hidden)
        // UNSPECIFIED = old daemon / not sampled — no card, defer to the skew banner.
        XCTAssertEqual(DaemonStore.localNetworkVerdict(grant: .unspecified), .hidden)
    }
}
