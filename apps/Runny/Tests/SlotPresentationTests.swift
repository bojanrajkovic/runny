import XCTest

@testable import Runny

import RunnyV1

final class CycleOrderTests: XCTestCase {
    func testSecureSSHSitsBetweenAwaitSSHAndMintJIT() {
        // The proto appends SECURE_SSH=12 for wire compat; cycle order is
        // the contract. Sorting by rawValue would misplace it.
        XCTAssertLessThan(
            Runny_V1_SlotState.awaitSsh.cycleIndex,
            Runny_V1_SlotState.secureSsh.cycleIndex
        )
        XCTAssertLessThan(
            Runny_V1_SlotState.secureSsh.cycleIndex,
            Runny_V1_SlotState.mintJit.cycleIndex
        )
        XCTAssertGreaterThan(
            Runny_V1_SlotState.secureSsh.rawValue,
            Runny_V1_SlotState.teardown.rawValue,
            "if this fails the proto renumbered and the explicit order needs a fresh look"
        )
    }

    func testDebugSitsBetweenJobAndTeardown() {
        // SLOT_STATE_DEBUG=13 is appended for wire compat but is the operator
        // hold between JOB and TEARDOWN in cycle order.
        XCTAssertLessThan(
            Runny_V1_SlotState.job.cycleIndex,
            Runny_V1_SlotState.debug.cycleIndex
        )
        XCTAssertLessThan(
            Runny_V1_SlotState.debug.cycleIndex,
            Runny_V1_SlotState.teardown.cycleIndex
        )
        XCTAssertEqual(Runny_V1_SlotState.debug.displayName, "DEBUG")
    }

    func testUnknownStatesSortLastAndRenderGracefully() {
        XCTAssertEqual(
            Runny_V1_SlotState.unspecified.cycleIndex,
            Runny_V1_SlotState.cycleOrder.count
        )
        XCTAssertEqual(Runny_V1_SlotState.UNRECOGNIZED(99).displayName, "STATE(99)")
        XCTAssertEqual(Runny_V1_SlotState.unspecified.displayName, "—")
    }

    func testEveryRealStateIsInCycleOrderExactlyOnce() {
        XCTAssertEqual(
            Set(Runny_V1_SlotState.cycleOrder).count,
            Runny_V1_SlotState.cycleOrder.count
        )
        for state in Runny_V1_SlotState.allCases
            where state != .unspecified
        {
            XCTAssertTrue(
                Runny_V1_SlotState.cycleOrder.contains(state),
                "\(state) missing from cycleOrder — new FSM state added?"
            )
        }
    }
}

final class NoteChainTests: XCTestCase {
    private func slot(
        state: Runny_V1_SlotState = .listening,
        enteredSecondsAgo: TimeInterval = 10,
        backoff: Int64 = 0,
        failures: UInt32 = 0,
        lastFailure: String = "",
        detail: String = ""
    ) -> Runny_V1_SlotStatus {
        var status = Runny_V1_SlotStatus()
        status.slot = "mac-1"
        status.state = state
        status.stateEntered = .init(
            date: Date(timeIntervalSinceNow: -enteredSecondsAgo))
        status.backoffSeconds = backoff
        status.consecutiveFailures = failures
        status.lastFailure = lastFailure
        status.detail = detail
        return status
    }

    func testBackoffPrependsRetryInFrontOfFailureText() {
        let s = slot(
            state: .backoff, enteredSecondsAgo: 5, backoff: 60,
            failures: 3, lastFailure: "boot deadline exceeded"
        )
        let note = SlotPresentation.note(s, now: Date())
        XCTAssertTrue(note.hasPrefix("retry in "), "retry-in is the useful number in BACKOFF: \(note)")
        XCTAssertTrue(note.contains("3 consecutive failures"))
        XCTAssertTrue(note.contains("boot deadline exceeded"))
    }

    func testRetryCountdownDisappearsOnceElapsed() {
        let s = slot(
            state: .backoff, enteredSecondsAgo: 120, backoff: 60,
            lastFailure: "x failed"
        )
        XCTAssertNil(SlotPresentation.retryIn(s, now: Date()))
        XCTAssertFalse(SlotPresentation.note(s, now: Date()).contains("retry in"))
    }

    func testNonBackoffNeverShowsRetry() {
        let s = slot(state: .boot, backoff: 60, lastFailure: "earlier failure")
        XCTAssertNil(SlotPresentation.retryIn(s, now: Date()))
    }

    func testDetailOverridesFailureTextEntirely() {
        let s = slot(
            failures: 2, lastFailure: "stale failure",
            detail: "2.1 GiB at 41 MiB/s"
        )
        XCTAssertEqual(SlotPresentation.note(s, now: Date()), "2.1 GiB at 41 MiB/s")
    }

    func testSingularFailureGrammar() {
        let s = slot(failures: 1, lastFailure: "ssh deadline")
        XCTAssertTrue(
            SlotPresentation.note(s, now: Date())
                .hasPrefix("1 consecutive failure; "))
    }

    func testCleanSlotHasEmptyNote() {
        XCTAssertEqual(SlotPresentation.note(slot(), now: Date()), "")
    }
}

final class PresentationFormattingTests: XCTestCase {
    func testDurationClampsAndScales() {
        XCTAssertEqual(SlotPresentation.duration(-5), "0s")
        XCTAssertEqual(SlotPresentation.duration(42), "42s")
        XCTAssertEqual(SlotPresentation.duration(90), "1m30s")
        XCTAssertEqual(SlotPresentation.duration(120), "2m")
        XCTAssertEqual(SlotPresentation.duration(3600), "1h")
        XCTAssertEqual(SlotPresentation.duration(3600 + 240), "1h4m")
    }

    func testStateLabelPausedAndWedged() {
        var status = Runny_V1_SlotStatus()
        status.state = .listening
        XCTAssertEqual(SlotPresentation.stateLabel(status), "LISTENING")
        status.paused = true
        XCTAssertEqual(SlotPresentation.stateLabel(status), "LISTENING*")
        status.wedged = true
        XCTAssertEqual(SlotPresentation.stateLabel(status), "WEDGED!")
    }

    func testDisplayNameFallsBackToSlotInBackoff() {
        var status = Runny_V1_SlotStatus()
        status.slot = "mac-2"
        XCTAssertEqual(SlotPresentation.displayName(status), "mac-2")
        status.runnerName = "junction-a1b2c3d4-mac-2-e48657d0"
        XCTAssertEqual(
            SlotPresentation.displayName(status),
            "junction-a1b2c3d4-mac-2-e48657d0"
        )
    }

    func testTimeInStateClampsNegative() {
        var status = Runny_V1_SlotStatus()
        status.stateEntered = .init(date: Date(timeIntervalSinceNow: 30))
        XCTAssertEqual(SlotPresentation.timeInState(status, now: Date()), 0)
    }
}

final class StatePhraseTests: XCTestCase {
    func testTransientStatesReadAsHumanPhrases() {
        XCTAssertEqual(Runny_V1_SlotState.ensureImage.phrase, "Pulling image")
        XCTAssertEqual(Runny_V1_SlotState.boot.phrase, "Booting")
        XCTAssertEqual(Runny_V1_SlotState.mintJit.phrase, "Registering runner")
        XCTAssertEqual(Runny_V1_SlotState.listening.phrase, "Listening")
    }

    func testUnknownStatePhraseDoesNotCrash() {
        XCTAssertEqual(Runny_V1_SlotState.UNRECOGNIZED(42).phrase, "State 42")
    }

    func testStatePhraseIsTheGuiVoiceWhileLabelStaysCliToken() {
        var status = Runny_V1_SlotStatus()
        status.state = .ensureImage
        // The two surfaces diverge by design: phrase humanizes, label mirrors
        // runnyctl's token verbatim.
        XCTAssertEqual(SlotPresentation.statePhrase(status), "Pulling image")
        XCTAssertEqual(SlotPresentation.stateLabel(status), "ENSURE_IMAGE")
    }

    func testWedgedOverridesPhraseEverywhere() {
        var status = Runny_V1_SlotStatus()
        status.state = .listening
        status.wedged = true
        XCTAssertEqual(SlotPresentation.statePhrase(status), "Wedged")
    }

    func testPausedIsNotFoldedIntoThePhrase() {
        // Paused is surfaced separately (chip / Info toggle), so unlike
        // stateLabel's "*" it must not leak into the phrase.
        var status = Runny_V1_SlotStatus()
        status.state = .listening
        status.paused = true
        XCTAssertEqual(SlotPresentation.statePhrase(status), "Listening")
        XCTAssertEqual(SlotPresentation.stateLabel(status), "LISTENING*")
    }
}

final class DoctorTitleTests: XCTestCase {
    func testKnownSlugsGetFriendlyTitles() {
        XCTAssertEqual(SlotPresentation.doctorTitle("config-drift").title, "Config drift")
        XCTAssertEqual(SlotPresentation.doctorTitle("macos-guest-cap").title, "macOS guest cap")
        XCTAssertEqual(SlotPresentation.doctorTitle("disk-headroom").title, "Disk headroom")
    }

    func testQualifiedNamesSplitTitleFromEntity() {
        let perm = SlotPresentation.doctorTitle("runner-perm:bojanrajkovic/mcp-paprika")
        XCTAssertEqual(perm.title, "Runner permission")
        XCTAssertEqual(perm.qualifier, "bojanrajkovic/mcp-paprika")

        let image = SlotPresentation.doctorTitle("image-resolve:linux")
        XCTAssertEqual(image.title, "Image resolve")
        XCTAssertEqual(image.qualifier, "linux")
    }

    func testUnknownSlugFallsBackToHumanizedHyphens() {
        let parsed = SlotPresentation.doctorTitle("some-new-check")
        XCTAssertEqual(parsed.title, "Some new check")
        XCTAssertNil(parsed.qualifier)
    }

    func testNoQualifierWhenNoColon() {
        XCTAssertNil(SlotPresentation.doctorTitle("platform").qualifier)
    }
}
