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
}
