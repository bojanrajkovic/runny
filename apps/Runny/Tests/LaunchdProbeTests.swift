import XCTest

@testable import Runny

/// The bounded label probe: `classify`'s result mapping (the A1 present/stopped/
/// absent table, stdout-only literal match) pinned directly against constructed
/// `CommandResult` values, plus a real end-to-end probe and — the load-bearing
/// part — that `BoundedProcess` honors its wall-clock bound on a command that
/// would otherwise run far longer.
final class LaunchdProbeTests: XCTestCase {
    // MARK: - Result mapping (the A1 table)

    func testRegisteredWhenLabelInStdout() {
        let label = "com.coderinserepeat.runnyd"
        let stdout = "system/\(label) = {\n\tstate = running\n\tprogram = /x\n}"
        let r = LaunchdProbe.classify(result: .exited(code: 0, stdout: stdout, stderr: ""), label: label)
        XCTAssertEqual(r, .registered)
    }

    func testStoppedButRegisteredStillReadsRegistered() {
        // A1: a bootstrapped-but-stopped system daemon still prints the label in
        // stdout with `state = not running`, so it reads .registered — the exact
        // case an exit-status read would silently miss.
        let label = "com.coderinserepeat.runnyd"
        let stdout = "system/\(label) = {\n\tstate = not running\n\tlast exit code = 0\n}"
        let r = LaunchdProbe.classify(result: .exited(code: 0, stdout: stdout, stderr: ""), label: label)
        XCTAssertEqual(r, .registered)
    }

    func testNotRegisteredWhenAbsentEvenThoughStderrEchoesLabel() {
        // The A1 stdout-only guard: an absent label yields EMPTY stdout and echoes
        // the label only in stderr. Searching the combined stream would
        // false-positive; searching stdout only correctly reads .notRegistered.
        let label = "com.coderinserepeat.runnyd"
        let stderr = "Could not find service \"\(label)\" in domain for system"
        let r = LaunchdProbe.classify(result: .exited(code: 113, stdout: "", stderr: stderr), label: label)
        XCTAssertEqual(r, .notRegistered)
    }

    func testTimedOutIsIndeterminate() {
        XCTAssertEqual(LaunchdProbe.classify(result: .timedOut, label: "x"), .indeterminate)
    }

    func testLaunchFailedIsIndeterminate() {
        XCTAssertEqual(LaunchdProbe.classify(result: .launchFailed("nope"), label: "x"), .indeterminate)
    }

    func testAmbiguousErrorIsIndeterminateNotNotRegistered() {
        // A non-"could not find" failure (permission, SIP) must defer, never read as
        // a clean absence — the app must not classify unmanaged off a probe error.
        let label = "com.coderinserepeat.runnyd"
        let r = LaunchdProbe.classify(
            result: .exited(code: 1, stdout: "", stderr: "Permission denied"), label: label
        )
        XCTAssertEqual(r, .indeterminate)
    }

    // MARK: - The real probe (target construction + BoundedProcess, end to end)

    func testRealProbeOfAnUnregisteredLabelReadsNotRegistered() async {
        // No fake seam anymore — probe() calls BoundedProcess directly, so this
        // proves the "system/<label>" argument construction against the real
        // launchctl: a label that can't exist yields the recognized "could not
        // find" absence, never .indeterminate.
        let label = "com.coderinserepeat.runny-test-\(UUID().uuidString)"
        let r = await LaunchdProbe.probe(label: label)
        XCTAssertEqual(r, .notRegistered)
    }

    func testRealRunnerCapturesStdout() async {
        let r = await BoundedProcess.run("/bin/echo", ["hello-probe"], timeout: .seconds(2), stdoutByteCap: 64 * 1024)
        guard case let .exited(code, stdout, _) = r else { return XCTFail("expected exited, got \(r)") }
        XCTAssertEqual(code, 0)
        XCTAssertTrue(stdout.contains("hello-probe"))
    }

    func testRealRunnerHonorsTheBoundOnAWedgedCommand() async {
        // A command that would run far longer than the bound must return .timedOut
        // well within it — the bounded-operation guarantee, proven WITHOUT hanging
        // the suite: the bound is tiny and the elapsed assert proves it fired rather
        // than waited the command out.
        let start = ContinuousClock.now
        let r = await BoundedProcess.run("/bin/sleep", ["30"], timeout: .milliseconds(150), stdoutByteCap: 64 * 1024)
        let elapsed = ContinuousClock.now - start
        XCTAssertEqual(r, .timedOut)
        XCTAssertLessThan(elapsed, .seconds(5), "the probe must honor its bound, not wait the command out")
    }
}
