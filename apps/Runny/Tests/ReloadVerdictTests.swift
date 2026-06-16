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
/// declared wedged; a v2 daemon is wedged only when frozen past the bound with no
/// slot active and the exit gate not held. Mirrors runnyctl's `streamDrain`
/// carve-out. Pure, so every branch is pinned without a live daemon.
final class DrainStallTests: XCTestCase {
    func testPreV2NeverStalls() {
        // Frozen far past the bound, but there is no drain_seq to measure against.
        XCTAssertFalse(DaemonStore.drainStalled(
            protocolVersion: 1, stalledFor: 1000, bound: 90, anySlotActive: false, exitHeld: false
        ))
    }

    func testV2StallsWhenFrozenPastBoundAndQuiescent() {
        XCTAssertTrue(DaemonStore.drainStalled(
            protocolVersion: 2, stalledFor: 91, bound: 90, anySlotActive: false, exitHeld: false
        ))
    }

    func testV2SuppressedWhileActiveOrHeld() {
        // A slot still working an active state is bounded daemon-side by its own
        // per-state deadline and a held exit gate is operator-actionable — not
        // hangs, so no stall even far past the bound.
        XCTAssertFalse(DaemonStore.drainStalled(
            protocolVersion: 2, stalledFor: 1000, bound: 90, anySlotActive: true, exitHeld: false
        ))
        XCTAssertFalse(DaemonStore.drainStalled(
            protocolVersion: 2, stalledFor: 1000, bound: 90, anySlotActive: false, exitHeld: true
        ))
    }

    func testV2WithinBoundIsNotYetStalled() {
        XCTAssertFalse(DaemonStore.drainStalled(
            protocolVersion: 2, stalledFor: 30, bound: 90, anySlotActive: false, exitHeld: false
        ))
    }
}

/// Slot activity drives the stall carve-out: any non-BACKOFF state is a slot
/// still working a daemon-bounded step, so the stall is suppressed for it; only a
/// fully quiescent fleet (all BACKOFF) can be called hung. Mirrors runnyctl's
/// `anySlotActive`.
final class SlotActivityTests: XCTestCase {
    private func slot(_ state: Runny_V1_SlotState) -> Runny_V1_SlotStatus {
        var s = Runny_V1_SlotStatus()
        s.state = state
        return s
    }

    func testProvisioningIsActive() {
        // A slot mid-PROVISION (180s daemon deadline) must read as active so the
        // 90s stall is suppressed rather than calling a healthy drain hung.
        XCTAssertTrue(DaemonStore.anySlotActive([slot(.backoff), slot(.provision)]))
    }

    func testAllBackoffIsQuiescent() {
        XCTAssertFalse(DaemonStore.anySlotActive([slot(.backoff), slot(.backoff)]))
    }

    func testWedgedSlotIsQuiescent() {
        // A wedged slot is a converged drain state even though it still reports an
        // underlying state like TEARDOWN — it must not read as active, or a
        // converged-but-not-exiting fleet would suppress the stall forever.
        var wedged = slot(.teardown)
        wedged.wedged = true
        XCTAssertFalse(DaemonStore.anySlotActive([slot(.backoff), wedged]))
    }

    func testEmptyIsQuiescent() {
        XCTAssertFalse(DaemonStore.anySlotActive([]))
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

/// The reload's job-in-flight seed: only a running JOB colors the verdict (a job
/// may have been interrupted), so the predicate that seeds the flag at acceptance
/// and refines it per snapshot must catch a JOB slot and ignore a pull or a debug
/// hold. Pinning it guards the contract that lets a daemon dying mid-drain still
/// warn correctly.
final class JobInFlightSeedTests: XCTestCase {
    private func slot(_ state: Runny_V1_SlotState) -> Runny_V1_SlotStatus {
        var s = Runny_V1_SlotStatus()
        s.state = state
        return s
    }

    func testDetectsRunningJob() {
        XCTAssertTrue(DaemonStore.anyJobRunning([slot(.debug), slot(.job)]))
    }

    func testIgnoresNonJobStates() {
        // A pull (ENSURE_IMAGE) or a debug hold is not an interrupted job.
        XCTAssertFalse(DaemonStore.anyJobRunning([slot(.ensureImage), slot(.debug)]))
    }

    func testEmptyIsNoJob() {
        XCTAssertFalse(DaemonStore.anyJobRunning([]))
    }
}

/// The pending-reload lifecycle: only an accepted reload arms or replaces the
/// tracked pending. A refusal or transport failure leaves an earlier accepted
/// reload's tracking intact, so a second reload clicked (and refused) mid-drain
/// can't cancel the first one's convergence verdict. Pure, so pinned directly.
final class PendingReloadLifecycleTests: XCTestCase {
    private func pending(_ boot: String) -> DaemonStore.PendingReload {
        DaemonStore.PendingReload(
            acceptingBootID: boot, priorStart: nil, wantSHA: "sha-\(boot)", acceptedAt: Date()
        )
    }

    func testAcceptedReplacesExistingPending() {
        let old = pending("A"), new = pending("B")
        XCTAssertEqual(DaemonStore.pendingAfterAttempt(existing: old, accepted: new), new)
    }

    func testRefusalKeepsExistingPending() {
        let old = pending("A")
        XCTAssertEqual(DaemonStore.pendingAfterAttempt(existing: old, accepted: nil), old)
    }

    func testRefusalWithNoPriorPendingStaysNil() {
        XCTAssertNil(DaemonStore.pendingAfterAttempt(existing: nil, accepted: nil))
    }
}

/// A reload that throws is ambiguous — a transport drop or deadline means the
/// daemon may have accepted it and begun draining — so the banner must not assert
/// a flat "reload failed". (A definitive gRPC rejection IS a real failure, but
/// that path is the established isDefinitiveRejection one and needs the gRPC
/// module to construct; the new behavior pinned here is the ambiguous case.)
final class ReloadThrowBannerTests: XCTestCase {
    private struct TransportDrop: Error {} // no gRPC code → ambiguous

    func testAmbiguousThrowDoesNotClaimFailure() {
        let banner = DaemonStore.reloadThrowBanner(TransportDrop())
        XCTAssertTrue(
            banner.contains("may have accepted it and started draining"),
            "ambiguous throw must surface the unknown-outcome guidance, got: \(banner)"
        )
        XCTAssertFalse(
            banner.contains("reload failed"),
            "an ambiguous throw must not assert a failure that may not have happened"
        )
    }
}
