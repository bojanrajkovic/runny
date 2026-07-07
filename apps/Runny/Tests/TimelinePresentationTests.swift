import XCTest

@testable import Runny

import RunnyV1
import SwiftProtobuf

final class TimelinePresentationTests: XCTestCase {
    private func slot(
        cycleID: String = "cycle-a", state: Runny_V1_SlotState = .listening, wedged: Bool = false
    ) -> Runny_V1_SlotStatus {
        var s = Runny_V1_SlotStatus()
        s.cycleID = cycleID
        s.state = state
        s.wedged = wedged
        return s
    }

    // MARK: - hasCurrent

    func testHasCurrentTrueForALiveNonBackoffState() {
        XCTAssertTrue(TimelinePresentation.hasCurrent(slot(state: .job)))
    }

    func testHasCurrentFalseWhenCycleIDEmpty() {
        XCTAssertFalse(TimelinePresentation.hasCurrent(slot(cycleID: "", state: .job)))
    }

    func testHasCurrentFalseInBackoff() {
        XCTAssertFalse(TimelinePresentation.hasCurrent(slot(state: .backoff)))
    }

    func testHasCurrentFalseWhenUnspecified() {
        XCTAssertFalse(TimelinePresentation.hasCurrent(slot(state: .unspecified)))
    }

    func testHasCurrentFalseWhenWedged() {
        // A wedged slot keeps a non-empty cycleID and TEARDOWN after its cycle
        // record already finished — must not render as a live cycle.
        XCTAssertFalse(TimelinePresentation.hasCurrent(slot(state: .teardown, wedged: true)))
    }

    // MARK: - isValid

    func testCurrentSelectionValidOnlyWhileHasCurrent() {
        XCTAssertTrue(TimelinePresentation.isValid(.current, hasCurrent: true, cycleIDs: []))
        XCTAssertFalse(TimelinePresentation.isValid(.current, hasCurrent: false, cycleIDs: []))
    }

    func testCycleSelectionValidOnlyWhileIDIsLoaded() {
        XCTAssertTrue(
            TimelinePresentation.isValid(.cycle("a"), hasCurrent: false, cycleIDs: ["a", "b"])
        )
        XCTAssertFalse(
            TimelinePresentation.isValid(.cycle("gone"), hasCurrent: false, cycleIDs: ["a", "b"])
        )
    }

    // MARK: - completedDurations

    // Built from seconds/nanos directly, not SwiftProtobuf's Date convenience
    // initializer — deliberately avoided elsewhere in this codebase (see
    // SlotPresentation.swift's dateValue comment) since it has shifted
    // across SwiftProtobuf versions.
    private func timestamp(_ epochSeconds: TimeInterval) -> Google_Protobuf_Timestamp {
        var t = Google_Protobuf_Timestamp()
        t.seconds = Int64(epochSeconds)
        return t
    }

    private func stateRecord(
        _ state: Runny_V1_SlotState, entered: TimeInterval, left: TimeInterval
    ) -> Runny_V1_StateRecord {
        var r = Runny_V1_StateRecord()
        r.state = state
        r.entered = timestamp(entered)
        r.left = timestamp(left)
        return r
    }

    func testCompletedDurationsComputesElapsedPerState() {
        var s = slot()
        s.activeCycleStates = [stateRecord(.clone, entered: 0, left: 10)]
        XCTAssertEqual(TimelinePresentation.completedDurations(for: s)[.clone], 10)
    }

    func testCompletedDurationsDropsNegativeElapsed() {
        // A clock anomaly (left before entered) must not surface a bogus
        // negative duration.
        var s = slot()
        s.activeCycleStates = [stateRecord(.clone, entered: 10, left: 5)]
        XCTAssertNil(TimelinePresentation.completedDurations(for: s)[.clone])
    }

    // MARK: - pipelinePath

    func testPipelinePathExcludesBackoff() {
        XCTAssertFalse(TimelinePresentation.pipelinePath(for: slot()).contains(.backoff))
    }

    func testPipelinePathExcludesDebugWhenNotArmedOrActive() {
        var s = slot(state: .listening)
        s.debugHoldArmed = false
        XCTAssertFalse(TimelinePresentation.pipelinePath(for: s).contains(.debug))
    }

    func testPipelinePathIncludesDebugWhenArmed() {
        var s = slot(state: .listening)
        s.debugHoldArmed = true
        XCTAssertTrue(TimelinePresentation.pipelinePath(for: s).contains(.debug))
    }

    func testPipelinePathIncludesDebugWhenCurrentlyInDebug() {
        let s = slot(state: .debug)
        XCTAssertTrue(TimelinePresentation.pipelinePath(for: s).contains(.debug))
    }

    // MARK: - pipelinePosition

    func testPipelinePositionClassifiesRelativeToCurrentIndex() {
        let cloneIndex = Runny_V1_SlotState.clone.cycleIndex
        XCTAssertEqual(TimelinePresentation.pipelinePosition(of: .clone, currentIndex: cloneIndex + 1), .passed)
        XCTAssertEqual(TimelinePresentation.pipelinePosition(of: .clone, currentIndex: cloneIndex), .current)
        XCTAssertEqual(TimelinePresentation.pipelinePosition(of: .clone, currentIndex: cloneIndex - 1), .pending)
    }

    // MARK: - pickerLabel

    func testPickerLabelIncludesCycleID() {
        var cycle = Runny_V1_CycleRecord()
        cycle.cycleID = "abcd1234"
        cycle.started = timestamp(Date().timeIntervalSince1970)
        XCTAssertTrue(TimelinePresentation.pickerLabel(cycle).contains("abcd1234"))
    }
}
