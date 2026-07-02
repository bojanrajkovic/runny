import XCTest

@testable import Runny

import RunnyV1

/// Mirrors the contract runnyctl's renderCycle tests pin: Result — not
/// Ending — gates success, Ending picks the verb for the benign non-success
/// endings, and a wedge (or a record from an older daemon with no ending)
/// stays a plain failure.
final class CycleVerdictTests: XCTestCase {
    private func record(result: String, ending: String = "") -> Runny_V1_CycleRecord {
        var rec = Runny_V1_CycleRecord()
        rec.result = result
        rec.ending = ending
        return rec
    }

    func testEndingPicksTheVerdict() {
        XCTAssertEqual(CycleVerdict(record(result: "success", ending: "success")), .success)
        XCTAssertEqual(CycleVerdict(record(result: "failure", ending: "recycle")), .recycle)
        XCTAssertEqual(CycleVerdict(record(result: "failure", ending: "shutdown")), .shutdown)
        XCTAssertEqual(CycleVerdict(record(result: "failure", ending: "failure")), .failure)
        // A wedge is a real failure — the guest survived force-stop — never
        // a benign ending.
        XCTAssertEqual(CycleVerdict(record(result: "failure", ending: "wedge")), .failure)
    }

    func testResultIsAuthoritativeForSuccess() {
        // A desynced record (corrupted or hand-edited cycle.json) must never
        // render a false ✓.
        XCTAssertEqual(CycleVerdict(record(result: "failure", ending: "success")), .failure)
    }

    func testOldRecordsWithoutEndingFallBackToResult() {
        XCTAssertEqual(CycleVerdict(record(result: "success")), .success)
        XCTAssertEqual(CycleVerdict(record(result: "failure")), .failure)
    }

    func testMarksAndVerbsMirrorRunnyctl() {
        XCTAssertEqual(CycleVerdict.success.mark, "✓")
        XCTAssertEqual(CycleVerdict.recycle.mark, "↻")
        XCTAssertEqual(CycleVerdict.shutdown.mark, "⏻")
        XCTAssertEqual(CycleVerdict.failure.mark, "✗")

        XCTAssertEqual(CycleVerdict.recycle.verb, "recycled by operator")
        XCTAssertEqual(CycleVerdict.shutdown.verb, "interrupted by daemon shutdown")
        XCTAssertEqual(CycleVerdict.failure.verb, "failed")
    }
}
