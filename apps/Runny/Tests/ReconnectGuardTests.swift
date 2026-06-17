import XCTest

@testable import Runny

/// The daemon-card Reconnect is `.disabled(store.reloadPending)` so a manual
/// re-dial can't run mid-drain and discard a live reload-convergence verdict.
///
/// `reloadPending` is the testable seam: it must read the live `pendingReload`,
/// not a constant, and a freshly built store (no reload sent) must report false
/// so Reconnect is available. Arming a real `pendingReload` needs the live RPC
/// flow (no pure seam, by design), so the disabled-while-pending direction is
/// the view's `.disabled` binding — verified in review, not a sham unit test.
@MainActor
final class ReconnectGuardTests: XCTestCase {
    func testFreshStoreReportsNoReloadPending() {
        let store = DaemonStore()
        XCTAssertFalse(
            store.reloadPending,
            "a store with no reload in flight must leave Reconnect enabled"
        )
    }
}
