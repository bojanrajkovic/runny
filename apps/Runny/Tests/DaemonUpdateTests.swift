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
            DaemonStore.daemonUpdate(agentInstalled: true, agentCanonical: true, runningBundleCanonical: true, appNewer: true, protocolBehind: false, daemonCore: "0.5.0", reloadPending: false, attempted: false),
            .available
        )
        // A brew/manual daemon (agent not app-installed) is never offered the
        // futile fleet-draining update — only the generic skew banner.
        XCTAssertEqual(
            DaemonStore.daemonUpdate(agentInstalled: false, agentCanonical: true, runningBundleCanonical: true, appNewer: true, protocolBehind: false, daemonCore: "0.5.0", reloadPending: false, attempted: false),
            .none
        )
        XCTAssertEqual(
            DaemonStore.daemonUpdate(agentInstalled: true, agentCanonical: true, runningBundleCanonical: true, appNewer: false, protocolBehind: false, daemonCore: "0.6.0", reloadPending: false, attempted: false),
            .none
        )
    }

    func testProtocolBehindIsSameCoreOlderProtocolOnly() {
        // Same core, daemon's protocol older than the app expects → the upgrade
        // window the version compare alone misses.
        XCTAssertTrue(DaemonStore.protocolBehind(
            appVersion: "0.6.0", daemonVersion: "0.6.0", daemonProtocol: 1, appExpectedProtocol: 2
        ))
        // Same core, same protocol → not behind.
        XCTAssertFalse(DaemonStore.protocolBehind(
            appVersion: "0.6.0", daemonVersion: "0.6.0", daemonProtocol: 2, appExpectedProtocol: 2
        ))
        // Different core is the version-mismatch axis, not the protocol one.
        XCTAssertFalse(DaemonStore.protocolBehind(
            appVersion: "0.6.0", daemonVersion: "0.5.0", daemonProtocol: 1, appExpectedProtocol: 2
        ))
    }

    func testUpdateOfferedForProtocolBehindWindow() {
        // Same version core but an older protocol: a reload moves launchd onto the
        // newer bundled binary, so an app-installed agent gets the update, not just
        // the generic skew banner.
        XCTAssertEqual(
            DaemonStore.daemonUpdate(agentInstalled: true, agentCanonical: true, runningBundleCanonical: true, appNewer: false, protocolBehind: true, daemonCore: "0.6.0", reloadPending: false, attempted: false),
            .available
        )
        // Neither axis ahead → nothing to offer.
        XCTAssertEqual(
            DaemonStore.daemonUpdate(agentInstalled: true, agentCanonical: true, runningBundleCanonical: true, appNewer: false, protocolBehind: false, daemonCore: "0.6.0", reloadPending: false, attempted: false),
            .none
        )
    }

    func testUpdateHiddenForForeignAgent() {
        // A foreign-path agent: a reload respawns the foreign BundleProgram, not
        // this app's bundled runnyd, so the update can't take — don't offer it.
        XCTAssertEqual(
            DaemonStore.daemonUpdate(agentInstalled: true, agentCanonical: false, runningBundleCanonical: true, appNewer: true, protocolBehind: false, daemonCore: "0.5.0", reloadPending: false, attempted: false),
            .none
        )
        // Canonical + ahead → still offered.
        XCTAssertEqual(
            DaemonStore.daemonUpdate(agentInstalled: true, agentCanonical: true, runningBundleCanonical: true, appNewer: true, protocolBehind: false, daemonCore: "0.5.0", reloadPending: false, attempted: false),
            .available
        )
    }

    func testUpdateHiddenWhenRunningBundleNotCanonical() {
        // A newer app run from Downloads while the installed agent points at the old
        // /Applications bundle: the agent is canonical, but appNewer compares the
        // RUNNING (Downloads) app — the reload respawns the OLD /Applications binary,
        // so the update can't take. Require the running bundle to be canonical too.
        XCTAssertEqual(
            DaemonStore.daemonUpdate(agentInstalled: true, agentCanonical: true, runningBundleCanonical: false, appNewer: true, protocolBehind: false, daemonCore: "0.5.0", reloadPending: false, attempted: false),
            .none
        )
        XCTAssertEqual(
            DaemonStore.daemonUpdate(agentInstalled: true, agentCanonical: true, runningBundleCanonical: true, appNewer: true, protocolBehind: false, daemonCore: "0.5.0", reloadPending: false, attempted: false),
            .available
        )
    }

    func testUninstallConfirmsWheneverLiveGuestStateIsUnknown() {
        // Connected with no live guests → provably safe, uninstall without a prompt.
        XCTAssertFalse(DaemonStore.uninstallNeedsConfirmation(connected: true, liveGuestSlots: []))
        // Live guests → always confirm.
        XCTAssertTrue(DaemonStore.uninstallNeedsConfirmation(connected: true, liveGuestSlots: ["mac-1"]))
        // NOT connected: an empty list is "no snapshot", not "no guest" — confirm
        // fail-safe so a disconnected uninstall can't silently abandon a job.
        XCTAssertTrue(DaemonStore.uninstallNeedsConfirmation(connected: false, liveGuestSlots: []))
    }

    func testUpdateInProgressThenDidNotTake() {
        XCTAssertEqual(
            DaemonStore.daemonUpdate(agentInstalled: true, agentCanonical: true, runningBundleCanonical: true, appNewer: true, protocolBehind: false, daemonCore: "0.5.0", reloadPending: true, attempted: true),
            .inProgress
        )
        // Reload resolved (not pending) but still app-newer after an attempt → loud,
        // named "didn't take", never a silent re-arm or a generic reload note.
        XCTAssertEqual(
            DaemonStore.daemonUpdate(agentInstalled: true, agentCanonical: true, runningBundleCanonical: true, appNewer: true, protocolBehind: false, daemonCore: "0.5.0", reloadPending: false, attempted: true),
            .didNotTake(daemonCore: "0.5.0")
        )
    }
}
