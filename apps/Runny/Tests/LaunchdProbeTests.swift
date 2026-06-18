import XCTest

@testable import Runny

/// The bounded label probe: the result mapping against a fake runner (the A1
/// present/stopped/absent table, stdout-only literal match), plus the real
/// `ProcessCommandRunner`'s capture and — the load-bearing part — that it honors
/// its wall-clock bound on a command that would otherwise run far longer.
final class LaunchdProbeTests: XCTestCase {
    private struct FakeRunner: CommandRunner {
        let result: CommandResult
        func run(_: String, _: [String], timeout _: Duration, stdoutByteCap _: Int) async -> CommandResult {
            result
        }
    }

    // MARK: - Result mapping (the A1 table)

    func testRegisteredWhenLabelInStdout() async {
        let label = "com.coderinserepeat.runnyd"
        let stdout = "gui/501/\(label) = {\n\tstate = running\n\tprogram = /x\n}"
        let r = await LaunchdProbe.probe(label: label, runner: FakeRunner(result: .exited(code: 0, stdout: stdout, stderr: "")))
        XCTAssertEqual(r, .registered)
    }

    func testStoppedButRegisteredStillReadsRegistered() async {
        // A1: `brew services stop` leaves the job bootstrapped; the label is still
        // in stdout with `state = not running`, so it reads .registered — the exact
        // case an exit-status read would silently miss.
        let label = "homebrew.mxcl.runny"
        let stdout = "gui/501/\(label) = {\n\tstate = not running\n\tlast exit code = 0\n}"
        let r = await LaunchdProbe.probe(label: label, runner: FakeRunner(result: .exited(code: 0, stdout: stdout, stderr: "")))
        XCTAssertEqual(r, .registered)
    }

    func testNotRegisteredWhenAbsentEvenThoughStderrEchoesLabel() async {
        // The A1 stdout-only guard: an absent label yields EMPTY stdout and echoes
        // the label only in stderr. Searching the combined stream would
        // false-positive; searching stdout only correctly reads .notRegistered.
        let label = "com.coderinserepeat.runnyd"
        let stderr = "Could not find service \"\(label)\" in domain for user gui: 501"
        let r = await LaunchdProbe.probe(label: label, runner: FakeRunner(result: .exited(code: 113, stdout: "", stderr: stderr)))
        XCTAssertEqual(r, .notRegistered)
    }

    func testTimedOutIsIndeterminate() async {
        let r = await LaunchdProbe.probe(label: "x", runner: FakeRunner(result: .timedOut))
        XCTAssertEqual(r, .indeterminate)
    }

    func testLaunchFailedIsIndeterminate() async {
        let r = await LaunchdProbe.probe(label: "x", runner: FakeRunner(result: .launchFailed("nope")))
        XCTAssertEqual(r, .indeterminate)
    }

    func testAmbiguousErrorIsIndeterminateNotNotRegistered() async {
        // A non-"could not find" failure (permission, SIP) must defer, never read as
        // a clean absence — the app must not classify unmanaged off a probe error.
        let label = "com.coderinserepeat.runnyd"
        let r = await LaunchdProbe.probe(label: label, runner: FakeRunner(result: .exited(code: 1, stdout: "", stderr: "Permission denied")))
        XCTAssertEqual(r, .indeterminate)
    }

    // MARK: - The real runner (capture + the bound)

    func testRealRunnerCapturesStdout() async {
        let r = await ProcessCommandRunner().run("/bin/echo", ["hello-probe"], timeout: .seconds(2), stdoutByteCap: 64 * 1024)
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
        let r = await ProcessCommandRunner().run("/bin/sleep", ["30"], timeout: .milliseconds(150), stdoutByteCap: 64 * 1024)
        let elapsed = ContinuousClock.now - start
        XCTAssertEqual(r, .timedOut)
        XCTAssertLessThan(elapsed, .seconds(5), "the probe must honor its bound, not wait the command out")
    }
}
