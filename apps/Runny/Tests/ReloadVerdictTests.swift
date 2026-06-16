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

/// The mid-drain stall gate: only a protocol-2 daemon publishes `drain_seq`, the
/// progress signal the stall rests on. A pre-2 daemon (no signal) must never be
/// declared wedged; a v2 daemon is wedged only when frozen past the bound with
/// nothing long-running or the exit gate held. Mirrors runnyctl's `streamDrain`
/// carve-out. Pure, so every branch is pinned without a live daemon.
final class DrainStallTests: XCTestCase {
    func testPreV2NeverStalls() {
        // Frozen far past the bound, but there is no drain_seq to measure against.
        XCTAssertFalse(DaemonStore.drainStalled(
            protocolVersion: 1, stalledFor: 1000, bound: 90, longRunning: false, exitHeld: false
        ))
    }

    func testV2StallsWhenFrozenPastBound() {
        XCTAssertTrue(DaemonStore.drainStalled(
            protocolVersion: 2, stalledFor: 91, bound: 90, longRunning: false, exitHeld: false
        ))
    }

    func testV2SuppressedWhileLongRunningOrHeld() {
        // A running job / image pull is daemon-bounded and a held exit gate is
        // operator-actionable — not hangs, so no stall even far past the bound.
        XCTAssertFalse(DaemonStore.drainStalled(
            protocolVersion: 2, stalledFor: 1000, bound: 90, longRunning: true, exitHeld: false
        ))
        XCTAssertFalse(DaemonStore.drainStalled(
            protocolVersion: 2, stalledFor: 1000, bound: 90, longRunning: false, exitHeld: true
        ))
    }

    func testV2WithinBoundIsNotYetStalled() {
        XCTAssertFalse(DaemonStore.drainStalled(
            protocolVersion: 2, stalledFor: 30, bound: 90, longRunning: false, exitHeld: false
        ))
    }
}

/// The respawn-silence deadline anchors at the later of acceptance and the last
/// snapshot — never at a snapshot that predates acceptance, so a stream already
/// near-stale when Reload was clicked can't bank that quiet against the respawn
/// wait. Pure, so pinned without a live stream.
final class RespawnSilenceTests: XCTestCase {
    func testPreAcceptanceSilenceIsNotBanked() {
        // The stream had been quiet ~94s when the reload was accepted 5s ago. The
        // bug measured from lastUpdate (~94s) and falsely expired; the anchor is
        // acceptance (5s ago), so it must NOT be expired.
        let now = Date()
        XCTAssertFalse(DaemonStore.respawnSilenceExpired(
            acceptedAt: now.addingTimeInterval(-5),
            lastUpdate: now.addingTimeInterval(-94), now: now, bound: 90
        ))
    }

    func testSilenceFromAcceptanceExpiresAfterBound() {
        // Daemon accepted, then died and never sent another snapshot: silence
        // accrues from acceptance and trips `bound` later.
        let now = Date()
        XCTAssertTrue(DaemonStore.respawnSilenceExpired(
            acceptedAt: now.addingTimeInterval(-91), lastUpdate: nil, now: now, bound: 90
        ))
    }

    func testPostAcceptanceSnapshotMovesAnchorForward() {
        // A fresh snapshot after acceptance resets the clock: 10s of silence since
        // it is well under bound, even though acceptance was long ago.
        let now = Date()
        XCTAssertFalse(DaemonStore.respawnSilenceExpired(
            acceptedAt: now.addingTimeInterval(-300),
            lastUpdate: now.addingTimeInterval(-10), now: now, bound: 90
        ))
    }
}
