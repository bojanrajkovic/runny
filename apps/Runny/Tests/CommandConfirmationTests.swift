import GRPC
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

    func testPauseConfirmsOnIDMembership() {
        let cmd = pending(.pause, id: "abc")
        XCTAssertTrue(
            DaemonStore.isConfirmed(cmd, by: slot(paused: true, recentApplied: ["abc"]))
        )
    }

    func testResumeConfirmsOnIDMembership() {
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

    func testPauseConfirmsOnAppliedIDEvenWhenSuperseded() {
        // The daemon records our id only when the pause actually applies. If a
        // resume immediately supersedes it — flipping paused back to false before
        // our next snapshot — the pause still ran, so its id in the history must
        // confirm it. A paused-direction belt here would reject a command that
        // applied and time it out into a false not-confirmed banner.
        let cmd = pending(.pause, id: "abc")
        XCTAssertTrue(
            DaemonStore.isConfirmed(cmd, by: slot(paused: false, recentApplied: ["abc"]))
        )
    }

    func testResumeConfirmsOnAppliedIDEvenWhenSuperseded() {
        // Symmetric: a resume that applied (id recorded) then was superseded by a
        // pause still confirms on membership.
        let cmd = pending(.resume, id: "abc")
        XCTAssertTrue(
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

    func testBannerRetractedOnlyForItsOwnCommand() {
        // The error banner is a single scalar shared across slots. A confirmation
        // retracts it only when the banner's provenance matches the confirming
        // command — never a different command's failure, and never a banner with
        // no owning command (unreachable, file-not-found: provenance nil).
        XCTAssertTrue(DaemonStore.bannerBelongs(to: "abc", confirmedID: "abc"))
        XCTAssertFalse(
            DaemonStore.bannerBelongs(to: "abc", confirmedID: "xyz"),
            "a different command's confirmation must not retract this banner"
        )
        XCTAssertFalse(
            DaemonStore.bannerBelongs(to: nil, confirmedID: "abc"),
            "an ownerless banner is never retracted by a command confirmation"
        )
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

/// The error classification that decides whether a failed command clears its
/// pending and surfaces at once (a definitive rejection) or holds the pending
/// for the confirmation watchdog (an ambiguous error). Getting this split wrong
/// either swallows a real failure or fakes a timeout over a live command.
final class ErrorClassificationTests: XCTestCase {
    func testDefinitiveRejectionCodes() {
        for code in [GRPCStatus.Code.notFound, .failedPrecondition, .invalidArgument] {
            XCTAssertTrue(
                GRPCStatus(code: code, message: nil).isDefinitiveRejection,
                "\(code) proves the command did not apply"
            )
        }
    }

    func testAmbiguousCodesAreNotDefinitive() {
        // A deadline or transport drop may have raced an applied command — the
        // pending must stand for the watchdog, never cleared as a proven failure.
        // Unavailable is included: grpc-swift uses it for both a full command
        // buffer (did not apply) and a dead transport (may have applied), so it's
        // ambiguous at this layer and must fail safe toward the watchdog.
        for code in [GRPCStatus.Code.unavailable, .deadlineExceeded, .cancelled, .unknown, .internalError] {
            XCTAssertFalse(
                GRPCStatus(code: code, message: nil).isDefinitiveRejection,
                "\(code) is ambiguous — the command may still have applied"
            )
        }
    }

    func testGRPCMessageIsExtracted() {
        let err = GRPCStatus(code: .failedPrecondition, message: "daemon is draining: wedge")
        XCTAssertEqual(err.grpcMessage, "daemon is draining: wedge")
    }
}
