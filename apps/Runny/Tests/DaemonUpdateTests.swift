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

    func testAutoApplyEligibilityIsSettingTimesAvailableTimesNotAttempted() {
        // Fires only when the setting is on, an update is actually on offer, and none
        // has been attempted this cycle. The OK-only gate + confirmed-ownership are
        // checked after this (async), in the trigger.
        XCTAssertTrue(DaemonStore.autoApplyShouldAttempt(settingOn: true, update: .available, attempted: false))
        // Setting off → button-only behavior, never auto.
        XCTAssertFalse(DaemonStore.autoApplyShouldAttempt(settingOn: false, update: .available, attempted: false))
        // Already attempted → the loop backstop: a didNotTake drops to the manual
        // "Try Again", never an auto-retry drain loop.
        XCTAssertFalse(DaemonStore.autoApplyShouldAttempt(settingOn: true, update: .available, attempted: true))
        // No update on offer → never (none / in-flight / didNotTake are not .available).
        XCTAssertFalse(DaemonStore.autoApplyShouldAttempt(settingOn: true, update: .none, attempted: false))
        XCTAssertFalse(DaemonStore.autoApplyShouldAttempt(settingOn: true, update: .inProgress, attempted: false))
        XCTAssertFalse(DaemonStore.autoApplyShouldAttempt(settingOn: true, update: .didNotTake(daemonCore: "0.5.0"), attempted: false))
    }

    func testAutoApplyWillIssueRejectsAnAlreadyAttemptedCycle() {
        // The will-issue check is re-evaluated at the commit point, AFTER the gate
        // probe await — so it must reject a fire whose cycle was already claimed while
        // it was suspended. Without the `attempted` term, two surfaces firing on one
        // settle let a straggler (resumed after the first reload cleared reloadInFlight)
        // drain the fleet a second time.
        XCTAssertTrue(DaemonStore.autoApplyWillIssue(clientPresent: true, reloadInFlight: false, attempted: false))
        // Already attempted this cycle → back out even though the client is up and no
        // reload is in flight (the straggler window).
        XCTAssertFalse(DaemonStore.autoApplyWillIssue(clientPresent: true, reloadInFlight: false, attempted: true))
        // No client / a reload already in flight → back out (mirrors performReload's guards).
        XCTAssertFalse(DaemonStore.autoApplyWillIssue(clientPresent: false, reloadInFlight: false, attempted: false))
        XCTAssertFalse(DaemonStore.autoApplyWillIssue(clientPresent: true, reloadInFlight: true, attempted: false))
    }
}
