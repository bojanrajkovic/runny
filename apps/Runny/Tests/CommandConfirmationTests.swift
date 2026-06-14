import XCTest

@testable import Runny

import RunnyV1

/// The pause/resume confirmation contract: a command confirms only when its id
/// is present in the daemon's `recentAppliedCommandIds` history, never on a
/// coincidentally-matching paused state. These exercise `DaemonStore.isConfirmed`
/// — the pure decision the live stream feeds.
final class CommandConfirmationTests: XCTestCase {
    private func slot(
        _ name: String = "runner-1", paused: Bool = false,
        recentApplied: [String] = [], cycleID: String = "cycle-a"
    ) -> Runny_V1_SlotStatus {
        var s = Runny_V1_SlotStatus()
        s.slot = name
        s.paused = paused
        s.recentAppliedCommandIds = recentApplied
        s.cycleID = cycleID
        return s
    }

    private func pending(
        _ kind: DaemonStore.PendingCommand.Kind, id: String,
        cycleID: String = "cycle-a"
    ) -> DaemonStore.PendingCommand {
        DaemonStore.PendingCommand(
            id: id, kind: kind, requestedAt: Date(timeIntervalSince1970: 0), cycleID: cycleID
        )
    }

    func testPauseConfirmsOnIDMembershipAndPausedDirection() {
        let cmd = pending(.pause, id: "abc")
        XCTAssertTrue(
            DaemonStore.isConfirmed(cmd, by: slot(paused: true, recentApplied: ["abc"]))
        )
    }

    func testResumeConfirmsOnIDMembershipAndResumedDirection() {
        let cmd = pending(.resume, id: "abc")
        XCTAssertTrue(
            DaemonStore.isConfirmed(cmd, by: slot(paused: false, recentApplied: ["abc"]))
        )
    }

    func testConfirmsWhenIDIsOneOfSeveralAppliedCommands() {
        // The membership point: concurrent clients (or a fast second command)
        // leave several ids in the history; ours need only be present, not last.
        // A scalar last-applied would have lost our id behind "theirs".
        let cmd = pending(.pause, id: "mine")
        XCTAssertTrue(
            DaemonStore.isConfirmed(
                cmd, by: slot(paused: true, recentApplied: ["theirs", "mine", "later"])
            )
        )
    }

    func testPauseDoesNotConfirmOnMatchingStateButAbsentID() {
        // The original bug: a paused slot must NOT confirm a pause whose id the
        // daemon never recorded (e.g. a periodic tick carrying paused=true).
        let cmd = pending(.pause, id: "mine")
        XCTAssertFalse(
            DaemonStore.isConfirmed(cmd, by: slot(paused: true, recentApplied: ["someone-elses"]))
        )
    }

    func testPauseDoesNotConfirmOnPresentIDButWrongDirection() {
        // The direction belt: a stale snapshot carrying our id but still showing
        // resumed must not confirm a pause.
        let cmd = pending(.pause, id: "abc")
        XCTAssertFalse(
            DaemonStore.isConfirmed(cmd, by: slot(paused: false, recentApplied: ["abc"]))
        )
    }

    func testResumeDoesNotConfirmOnPresentIDButWrongDirection() {
        let cmd = pending(.resume, id: "abc")
        XCTAssertFalse(
            DaemonStore.isConfirmed(cmd, by: slot(paused: true, recentApplied: ["abc"]))
        )
    }

    func testPreRequestSnapshotWithEmptyHistoryDoesNotConfirm() {
        // The snapshot in flight when the command was issued carries no id yet.
        let cmd = pending(.pause, id: "abc")
        XCTAssertFalse(
            DaemonStore.isConfirmed(cmd, by: slot(paused: true, recentApplied: []))
        )
    }

    func testDaemonRestartClearsHistorySoCommandDoesNotConfirm() {
        // A restarted daemon comes up with an empty history; the random id
        // can't collide, so the command stays pending until it times out.
        let cmd = pending(.pause, id: "abc")
        XCTAssertFalse(
            DaemonStore.isConfirmed(cmd, by: slot(paused: true, recentApplied: []))
        )
    }

    func testMissingSlotDoesNotConfirm() {
        XCTAssertFalse(DaemonStore.isConfirmed(pending(.pause, id: "abc"), by: nil))
        XCTAssertFalse(DaemonStore.isConfirmed(pending(.resume, id: "abc"), by: nil))
        XCTAssertFalse(DaemonStore.isConfirmed(pending(.recycle, id: "abc"), by: nil))
    }

    func testRecycleConfirmsOnCycleChangeIgnoringID() {
        // Recycle has no echoed id — the daemon carries none on RecycleRequest —
        // so it confirms purely on the cycle advancing.
        let cmd = pending(.recycle, id: "unused", cycleID: "cycle-a")
        XCTAssertTrue(
            DaemonStore.isConfirmed(cmd, by: slot(cycleID: "cycle-b"))
        )
        XCTAssertFalse(
            DaemonStore.isConfirmed(cmd, by: slot(cycleID: "cycle-a"))
        )
    }
}

/// `RecentlyConfirmed` is the bounded register that lets a snapshot's
/// confirmation swallow a single straggling RPC error for the same command —
/// without ever suppressing a later, genuine failure on a different one.
final class RecentlyConfirmedTests: XCTestCase {
    func testConsumeReturnsTrueExactlyOncePerNotedID() {
        var register = DaemonStore.RecentlyConfirmed()
        register.note("abc")
        XCTAssertTrue(register.consume("abc"), "the noted command's straggling error is swallowed")
        XCTAssertFalse(
            register.consume("abc"),
            "consume is once-only — a second error for the same id must still surface"
        )
    }

    func testConsumeOfUnnotedIDReturnsFalse() {
        var register = DaemonStore.RecentlyConfirmed()
        XCTAssertFalse(register.consume("never-noted"))
        register.note("abc")
        XCTAssertFalse(
            register.consume("xyz"),
            "an unrelated confirmation must not suppress a different command's error"
        )
    }

    func testConsumeIsPerIDNotGlobal() {
        var register = DaemonStore.RecentlyConfirmed()
        register.note("a")
        register.note("b")
        XCTAssertTrue(register.consume("b"))
        XCTAssertTrue(register.consume("a"), "consuming one id leaves the others intact")
    }

    func testOldestIDsEvictedPastCap() {
        var register = DaemonStore.RecentlyConfirmed(cap: 2)
        register.note("1")
        register.note("2")
        register.note("3") // pushes the window past the cap, evicting "1"
        XCTAssertFalse(register.consume("1"), "the oldest id past the cap is dropped")
        XCTAssertTrue(register.consume("2"))
        XCTAssertTrue(register.consume("3"))
    }
}
