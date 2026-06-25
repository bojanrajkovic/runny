import XCTest

@testable import Runny

/// The pure surface of the config-compat gate: parsing runnyd's `-test-config`
/// JSON verdict. The exec (`probe`) is the thin live-only shell; everything that
/// DECIDES — and the safety property that unparseable output never becomes a
/// silent OK — is pinned here without launching a process.
final class ConfigCompatGateTests: XCTestCase {
    func testParseOK() {
        let r = ConfigCompatGate.parseVerdict(#"{"status":"ok","errors":[],"warnings":[]}"#)
        XCTAssertEqual(r, .verdict(ConfigCompatVerdict(status: .ok, errors: [], warnings: [])))
    }

    func testParseWarn() {
        let json = #"{"status":"warn","errors":[],"warnings":[{"kind":"deadline-too-short","message":"deadlines.await_ssh is 1s, below the 2s floor"}]}"#
        guard case let .verdict(v) = ConfigCompatGate.parseVerdict(json) else {
            return XCTFail("want a verdict")
        }
        XCTAssertEqual(v.status, .warn)
        XCTAssertTrue(v.errors.isEmpty)
        XCTAssertEqual(v.warnings.count, 1)
        XCTAssertEqual(v.warnings.first?.kind, "deadline-too-short")
    }

    func testParseError() {
        let json = #"{"status":"error","errors":["macos-guest-cap: darwin pools total 3 slots"],"warnings":[]}"#
        guard case let .verdict(v) = ConfigCompatGate.parseVerdict(json) else {
            return XCTFail("want a verdict")
        }
        XCTAssertEqual(v.status, .error)
        XCTAssertEqual(v.errors.count, 1)
        XCTAssertTrue(v.errors.first?.contains("macos-guest-cap") ?? false)
    }

    func testMalformedOutputIsUnavailableNeverOK() {
        // A gate that can't parse must NOT fabricate an OK — unavailable is blocking.
        for bad in ["", "not json", #"{"status":"#, #"{"unexpected":true}"#] {
            guard case .unavailable = ConfigCompatGate.parseVerdict(bad) else {
                return XCTFail("malformed \(bad.debugDescription) must be unavailable, never a verdict")
            }
        }
    }

    func testUnknownStatusIsUnavailable() {
        // An unmodeled status fails the enum decode → unavailable, never a guess.
        guard case .unavailable = ConfigCompatGate.parseVerdict(#"{"status":"maybe","errors":[],"warnings":[]}"#) else {
            return XCTFail("an unknown status must be unavailable")
        }
    }

    // MARK: - update gate (OK proceed / Warn confirm / Error+unavailable block)

    func testUpdateGateOKProceeds() {
        let g = ConfigCompatGate.updateGate(for: .verdict(.init(status: .ok, errors: [], warnings: [])))
        XCTAssertEqual(g, .proceed)
    }

    func testUpdateGateWarnConfirmsWithWarnings() {
        let w = ConfigCompatVerdict.Warning(kind: "deadline-too-short", message: "await_ssh is 1s")
        let g = ConfigCompatGate.updateGate(for: .verdict(.init(status: .warn, errors: [], warnings: [w])))
        XCTAssertEqual(g, .confirm([w]))
    }

    func testUpdateGateErrorBlocksWithDetail() {
        let g = ConfigCompatGate.updateGate(for: .verdict(.init(status: .error, errors: ["macos-guest-cap: over"], warnings: [])))
        guard case let .block(msg) = g else { return XCTFail("error must block") }
        XCTAssertTrue(msg.contains("macos-guest-cap"))
    }

    func testUpdateGateErrorWithNoMessagesStillBlocksNonEmpty() {
        guard case let .block(msg) = ConfigCompatGate.updateGate(for: .verdict(.init(status: .error, errors: [], warnings: []))) else {
            return XCTFail("error must block")
        }
        XCTAssertFalse(msg.isEmpty)
    }

    func testUpdateGateUnavailableBlocks() {
        // A gate that can't speak must block — never silently proceed to a reload.
        guard case let .block(msg) = ConfigCompatGate.updateGate(for: .unavailable("runnyd timed out")) else {
            return XCTFail("unavailable must block")
        }
        XCTAssertTrue(msg.contains("runnyd timed out"))
    }

    // MARK: - commit re-gate (re-confirm a changed verdict, never silently apply)

    private var w1: ConfigCompatVerdict.Warning { .init(kind: "deadline-too-short", message: "await_ssh 1s") }
    private var w2: ConfigCompatVerdict.Warning { .init(kind: "resource-overcommit", message: "512 cores") }

    func testCommitGateOKProceeds() {
        XCTAssertEqual(ConfigCompatGate.commitGate(.proceed, confirmedWarnings: []), .proceed)
        // A Warn-at-click that improved to OK by commit still proceeds.
        XCTAssertEqual(ConfigCompatGate.commitGate(.proceed, confirmedWarnings: [w1]), .proceed)
    }

    func testCommitGateBlocks() {
        XCTAssertEqual(ConfigCompatGate.commitGate(.block("over cap"), confirmedWarnings: [w1]), .block("over cap"))
    }

    func testCommitGateSameWarnProceedsNoReconfirmLoop() {
        // The operator already confirmed exactly these warnings — proceed, or a stable
        // Warn config could never be updated (it would re-confirm forever).
        XCTAssertEqual(ConfigCompatGate.commitGate(.confirm([w1]), confirmedWarnings: [w1]), .proceed)
    }

    func testCommitGateNewWarnReconfirms() {
        // OK at click → Warn at commit: surface the unseen warnings, don't apply.
        XCTAssertEqual(ConfigCompatGate.commitGate(.confirm([w1]), confirmedWarnings: []), .reconfirm([w1]))
        // Warn at click → a DIFFERENT Warn at commit: re-confirm the new set.
        XCTAssertEqual(ConfigCompatGate.commitGate(.confirm([w2]), confirmedWarnings: [w1]), .reconfirm([w2]))
    }
}
