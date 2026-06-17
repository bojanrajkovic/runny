import XCTest

@testable import Runny

/// The post-upgrade daemon-update verdicts: the version-direction compare the
/// symmetric skew verdict doesn't report, and the available/inProgress/didNotTake
/// matrix. Pure → no live daemon.
final class DaemonUpdateTests: XCTestCase {
    func testSemverComparesNumericallyNotLexically() {
        XCTAssertTrue(DaemonStore.semverGreater("0.6.0", "0.5.0"))
        XCTAssertFalse(DaemonStore.semverGreater("0.5.0", "0.6.0"))
        XCTAssertFalse(DaemonStore.semverGreater("0.6.0", "0.6.0"))
        // The lexical trap: "0.10.0" sorts before "0.9.0" as strings, but 10 > 9.
        XCTAssertTrue(DaemonStore.semverGreater("0.10.0", "0.9.0"))
    }

    func testAppNewerRequiresAStampedAppAndAKnownDaemon() {
        XCTAssertTrue(DaemonStore.appNewerThanDaemon(appVersion: "0.6.0", daemonVersion: "0.5.0"))
        XCTAssertFalse(DaemonStore.appNewerThanDaemon(appVersion: "0.5.0", daemonVersion: "0.6.0"))
        // A dev build (unstamped 0.0.0) can't meaningfully update anything.
        XCTAssertFalse(DaemonStore.appNewerThanDaemon(appVersion: "0.0.0", daemonVersion: "0.5.0"))
        // No daemon version yet → nothing to compare.
        XCTAssertFalse(DaemonStore.appNewerThanDaemon(appVersion: "0.6.0", daemonVersion: ""))
    }

    func testUpdateOfferedOnlyToAppInstalledNewerAgent() {
        XCTAssertEqual(
            DaemonStore.daemonUpdate(agentInstalled: true, appNewer: true, daemonCore: "0.5.0", reloadPending: false, attempted: false),
            .available
        )
        // A brew/manual daemon (agent not app-installed) is never offered the
        // futile fleet-draining update — only the generic skew banner.
        XCTAssertEqual(
            DaemonStore.daemonUpdate(agentInstalled: false, appNewer: true, daemonCore: "0.5.0", reloadPending: false, attempted: false),
            .none
        )
        XCTAssertEqual(
            DaemonStore.daemonUpdate(agentInstalled: true, appNewer: false, daemonCore: "0.6.0", reloadPending: false, attempted: false),
            .none
        )
    }

    func testUpdateInProgressThenDidNotTake() {
        XCTAssertEqual(
            DaemonStore.daemonUpdate(agentInstalled: true, appNewer: true, daemonCore: "0.5.0", reloadPending: true, attempted: true),
            .inProgress
        )
        // Reload resolved (not pending) but still app-newer after an attempt → loud,
        // named "didn't take", never a silent re-arm or a generic reload note.
        XCTAssertEqual(
            DaemonStore.daemonUpdate(agentInstalled: true, appNewer: true, daemonCore: "0.5.0", reloadPending: false, attempted: true),
            .didNotTake(daemonCore: "0.5.0")
        )
    }
}
