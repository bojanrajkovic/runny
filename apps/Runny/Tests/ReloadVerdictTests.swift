import XCTest

@testable import Runny

import RunnyV1

/// The reload verdict taxonomy: given a respawned daemon's status against the
/// hash the reload validated, classify the outcome. The contract that matters —
/// only config drift is a `.failure` (operator must act); everything else is a
/// success or a degraded-but-ok warning. Pure, so every branch is pinned without
/// a live daemon. Mirrors runnyctl's `respawnVerdict`.
final class ReloadVerdictTests: XCTestCase {
    private let want = "4a5b6c7d8e9f00112233445566778899aabbccddeeff00112233445566778899"
    private let other = "ffffffffffff00112233445566778899aabbccddeeff00112233445566778899"

    func testProtocolBelowTwoCannotVerify() {
        let outcome = DaemonStore.respawnVerdict(
            protocolVersion: 1, gotSHA: "", wantSHA: want, jobInFlight: false, reDraining: ""
        )
        XCTAssertEqual(outcome.severity, .warning)
        XCTAssertTrue(outcome.text.contains("doesn't report its running config hash"))
    }

    func testConfigDriftIsAFailure() {
        let outcome = DaemonStore.respawnVerdict(
            protocolVersion: 2, gotSHA: other, wantSHA: want, jobInFlight: false, reDraining: ""
        )
        XCTAssertEqual(outcome.severity, .failure, "loading a different file is the one actionable failure")
        XCTAssertTrue(outcome.text.contains("NOT the config you reloaded"))
    }

    func testRespawnOnValidatedConfigIsClean() {
        let outcome = DaemonStore.respawnVerdict(
            protocolVersion: 2, gotSHA: want, wantSHA: want, jobInFlight: false, reDraining: ""
        )
        XCTAssertEqual(outcome.severity, .success)
        XCTAssertTrue(outcome.text.contains("respawned on config"))
    }

    func testJobInFlightIsAWarning() {
        // Right config, but a job was still running as the daemon went down —
        // the config IS live, but the job may have been interrupted.
        let outcome = DaemonStore.respawnVerdict(
            protocolVersion: 2, gotSHA: want, wantSHA: want, jobInFlight: true, reDraining: ""
        )
        XCTAssertEqual(outcome.severity, .warning)
        XCTAssertTrue(outcome.text.contains("went down with a job still running"))
    }

    func testReDrainingIsSurfaced() {
        // A new daemon already draining again must say so, so the operator isn't
        // surprised that another reload is needed.
        let outcome = DaemonStore.respawnVerdict(
            protocolVersion: 2, gotSHA: want, wantSHA: want, jobInFlight: false,
            reDraining: "wedged guest"
        )
        XCTAssertTrue(outcome.text.contains("already draining again: wedged guest"))
    }
}

/// A refused reload renders the failed checks and — when a drain is already
/// running — the loud warning that the respawn WILL load the invalid file.
final class ReloadRefusalTests: XCTestCase {
    private func check(_ name: String, _ detail: String) -> Runny_V1_DoctorCheck {
        var c = Runny_V1_DoctorCheck()
        c.name = name
        c.detail = detail
        c.ok = false
        return c
    }

    func testRefusalListsFailedChecks() {
        var resp = Runny_V1_ReloadResponse()
        resp.accepted = false
        resp.failedChecks = [check("config-parse", "yaml: line 3: bad node")]
        let text = DaemonStore.describeRefusal(resp)
        XCTAssertTrue(text.contains("the running daemon is unchanged"))
        XCTAssertTrue(text.contains("config-parse: yaml: line 3: bad node"))
        XCTAssertFalse(text.contains("WARNING: the daemon is already draining"))
    }

    func testRefusalWhileDrainingWarnsTheRespawnWillLoadIt() {
        var resp = Runny_V1_ReloadResponse()
        resp.accepted = false
        resp.draining = "config reload (SIGHUP)"
        resp.failedChecks = [check("config-parse", "bad yaml")]
        let text = DaemonStore.describeRefusal(resp)
        XCTAssertTrue(text.contains("WARNING: the daemon is already draining (config reload (SIGHUP))"))
        XCTAssertTrue(text.contains("respawn WILL load this invalid config"))
    }
}
