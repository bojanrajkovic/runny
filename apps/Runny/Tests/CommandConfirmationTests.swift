import XCTest

@testable import Runny

import RunnyV1

/// The pause/resume confirmation contract: a command confirms only on an exact
/// match of the daemon's echoed `lastAppliedCommandID`, never on a
/// coincidentally-matching paused state. These exercise `DaemonStore.isConfirmed`
/// — the pure decision the live stream feeds.
final class CommandConfirmationTests: XCTestCase {
    private func slot(
        _ name: String = "runner-1", paused: Bool = false,
        lastApplied: String = "", cycleID: String = "cycle-a"
    ) -> Runny_V1_SlotStatus {
        var s = Runny_V1_SlotStatus()
        s.slot = name
        s.paused = paused
        s.lastAppliedCommandID = lastApplied
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

    func testPauseConfirmsOnExactIDMatchAndPausedDirection() {
        let cmd = pending(.pause, id: "abc")
        XCTAssertTrue(
            DaemonStore.isConfirmed(cmd, by: slot(paused: true, lastApplied: "abc"))
        )
    }

    func testResumeConfirmsOnExactIDMatchAndResumedDirection() {
        let cmd = pending(.resume, id: "abc")
        XCTAssertTrue(
            DaemonStore.isConfirmed(cmd, by: slot(paused: false, lastApplied: "abc"))
        )
    }

    func testPauseDoesNotConfirmOnMatchingStateButDifferentID() {
        // The original bug: a paused slot must NOT confirm a pause whose id the
        // daemon never echoed (e.g. a periodic tick carrying paused=true).
        let cmd = pending(.pause, id: "mine")
        XCTAssertFalse(
            DaemonStore.isConfirmed(cmd, by: slot(paused: true, lastApplied: "someone-elses"))
        )
    }

    func testPauseDoesNotConfirmOnMatchingIDButWrongDirection() {
        // The direction belt: a stale snapshot echoing our id but still showing
        // resumed must not confirm a pause.
        let cmd = pending(.pause, id: "abc")
        XCTAssertFalse(
            DaemonStore.isConfirmed(cmd, by: slot(paused: false, lastApplied: "abc"))
        )
    }

    func testResumeDoesNotConfirmOnMatchingIDButWrongDirection() {
        let cmd = pending(.resume, id: "abc")
        XCTAssertFalse(
            DaemonStore.isConfirmed(cmd, by: slot(paused: true, lastApplied: "abc"))
        )
    }

    func testPreRequestSnapshotWithEmptyRegisterDoesNotConfirm() {
        // The snapshot in flight when the command was issued carries no id yet.
        let cmd = pending(.pause, id: "abc")
        XCTAssertFalse(
            DaemonStore.isConfirmed(cmd, by: slot(paused: true, lastApplied: ""))
        )
    }

    func testDaemonRestartClearsRegisterSoCommandDoesNotConfirm() {
        // A restarted daemon comes up with an empty register; the random id
        // can't collide, so the command stays pending until it times out.
        let cmd = pending(.pause, id: "abc")
        XCTAssertFalse(
            DaemonStore.isConfirmed(cmd, by: slot(paused: true, lastApplied: ""))
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
