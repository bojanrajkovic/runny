import XCTest

@testable import Runny

/// The pure mappers behind the CLI-install model: the verify→state mapping, the
/// osascript exit→outcome recovery, the translocation predicate, and the shell/
/// AppleScript escaping that keeps a path with spaces or quotes from breaking out
/// of the one privileged command. The FileManager/osascript execution itself is
/// the thin untested shell (live-machine only); everything that DECIDES is here.
final class CLIInstallModelTests: XCTestCase {
    // MARK: - verify → state

    func testStateForVerify() {
        XCTAssertEqual(CLIInstallModel.stateForVerify(.installed), .installed)
        XCTAssertEqual(CLIInstallModel.stateForVerify(.installedButNotOnPath), .installedButNotOnPath)
        // A read-back that resolves elsewhere or finds nothing despite a "success"
        // return is a loud failure, never a green check — the confirm-from-disk rule.
        if case .failed = CLIInstallModel.stateForVerify(.mismatch) {} else { XCTFail("mismatch must fail loudly") }
        if case .failed = CLIInstallModel.stateForVerify(.missing) {} else { XCTFail("missing must fail loudly") }
    }

    // MARK: - osascript exit → outcome

    func testOutcomeOk() {
        XCTAssertEqual(CLIInstallModel.outcomeForOsascript(exitCode: 0, stderr: ""), .ok)
    }

    func testOutcomeCancelled() {
        // A user-dismissed admin prompt is AppleScript error -128.
        XCTAssertEqual(
            CLIInstallModel.outcomeForOsascript(exitCode: 1, stderr: "execution error: User canceled. (-128)"),
            .cancelled
        )
    }

    func testOutcomeRefusedForeign() {
        // The foreign guard inside `do shell script` exits 3; osascript surfaces it
        // as the trailing parenthesized code.
        XCTAssertEqual(
            CLIInstallModel.outcomeForOsascript(exitCode: 1, stderr: "execution error: ... (3)"),
            .refusedForeign
        )
    }

    func testOutcomeFailed() {
        if case .failed = CLIInstallModel.outcomeForOsascript(exitCode: 1, stderr: "some other error (5)") {} else {
            XCTFail("an unrecognized nonzero exit must be a loud failure")
        }
        // A "(3)" embedded earlier must NOT be read as a foreign-refusal when the
        // real trailing exit code is something else — the precise-parse guard that
        // keeps a genuine failure from being silently downgraded to .conflict.
        if case .failed = CLIInstallModel.outcomeForOsascript(exitCode: 1, stderr: "ln: /Volumes/Disk (3)/x: failed (1)") {} else {
            XCTFail("embedded (3) must not misclassify a real failure as refusedForeign")
        }
        // Empty stderr still yields a non-empty message (no blank failure).
        if case let .failed(m) = CLIInstallModel.outcomeForOsascript(exitCode: 2, stderr: "") {
            XCTAssertFalse(m.isEmpty)
        } else {
            XCTFail("expected failed")
        }
    }

    func testTrailingParenCode() {
        XCTAssertEqual(CLIInstallModel.trailingParenCode("execution error: ... (3)"), 3)
        XCTAssertEqual(CLIInstallModel.trailingParenCode("User canceled. (-128)"), -128)
        // The LAST parenthesized code wins; an earlier "(3)" is ignored.
        XCTAssertEqual(CLIInstallModel.trailingParenCode("path (3)/x failed (1)"), 1)
        XCTAssertNil(CLIInstallModel.trailingParenCode("no trailing code here"))
        XCTAssertNil(CLIInstallModel.trailingParenCode(""))
    }

    func testPathContainsNormalizesTrailingSlash() {
        XCTAssertTrue(CLIInstallModel.pathContains("/usr/bin:/usr/local/bin:/bin", dir: "/usr/local/bin"))
        // A trailing slash on the PATH entry (or the queried dir) is the same dir.
        XCTAssertTrue(CLIInstallModel.pathContains("/usr/bin:/usr/local/bin/:/bin", dir: "/usr/local/bin"))
        XCTAssertTrue(CLIInstallModel.pathContains("/usr/bin:/usr/local/bin:/bin", dir: "/usr/local/bin/"))
        XCTAssertFalse(CLIInstallModel.pathContains("/usr/bin:/opt/homebrew/bin", dir: "/usr/local/bin"))
        XCTAssertFalse(CLIInstallModel.pathContains("", dir: "/usr/local/bin"))
    }

    // MARK: - translocation

    func testIsTranslocated() {
        XCTAssertTrue(CLIInstallModel.isTranslocated("/private/var/folders/ab/cd/T/AppTranslocation/UUID/d/Runny.app"))
        XCTAssertTrue(CLIInstallModel.isTranslocated("/private/var/folders/xy/z/Runny.app"))
        XCTAssertFalse(CLIInstallModel.isTranslocated("/Applications/Runny.app"))
        XCTAssertFalse(CLIInstallModel.isTranslocated("/Users/me/Applications/Runny.app"))
    }

    // MARK: - shell + AppleScript escaping (security-sensitive)

    func testShellSingleQuotePlain() {
        XCTAssertEqual(CLIInstallModel.shellSingleQuote("/Applications/Runny.app"), "'/Applications/Runny.app'")
    }

    func testShellSingleQuoteWithSpace() {
        XCTAssertEqual(
            CLIInstallModel.shellSingleQuote("/Users/me/My Apps/Runny.app/Contents/MacOS/runnyctl"),
            "'/Users/me/My Apps/Runny.app/Contents/MacOS/runnyctl'"
        )
    }

    func testShellSingleQuoteEscapesQuote() {
        // A single quote in the path must close-escape-reopen, never break out.
        XCTAssertEqual(CLIInstallModel.shellSingleQuote("/a'b"), "'/a'\\''b'")
    }

    func testAppleScriptEscaping() {
        // Backslashes and double-quotes are escaped for the AppleScript string layer.
        XCTAssertEqual(
            CLIInstallModel.appleScript(doShell: "echo \"hi\" \\ there"),
            "do shell script \"echo \\\"hi\\\" \\\\ there\" with administrator privileges"
        )
    }

    func testInstallScriptStructure() {
        let s = CLIInstallModel.installScript(target: "/Applications/Runny.app/Contents/MacOS/runnyctl")
        XCTAssertTrue(s.contains("with administrator privileges"))
        // Non-forcing create (single-quoted target through the AppleScript layer):
        // never `ln -sfn`, whose -f would clobber a foreign file that raced in after
        // the guard.
        XCTAssertTrue(s.contains("ln -s '/Applications/Runny.app/Contents/MacOS/runnyctl'"))
        XCTAssertFalse(s.contains("ln -sfn"))
        // The write-time guard removes ONLY a Runny.app link, then refuses (exit 3)
        // anything still at the path — a foreign file, or one that raced in.
        XCTAssertTrue(s.contains("*/Runny.app/Contents/MacOS/runnyctl) rm -f"))
        XCTAssertTrue(s.contains("if [ -e /usr/local/bin/runnyctl ] || [ -L /usr/local/bin/runnyctl ]; then exit 3"))
        // mkdir -p so a missing /usr/local/bin is created under escalation.
        XCTAssertTrue(s.contains("mkdir -p /usr/local/bin"))
    }

    func testResolvedLinkFollowsDanglingRunnyTarget() throws {
        let fm = FileManager.default
        let dir = fm.temporaryDirectory.appendingPathComponent("runny-resolvedlink-\(UUID().uuidString)")
        try fm.createDirectory(at: dir, withIntermediateDirectories: true)
        defer { try? fm.removeItem(at: dir) }
        let link = dir.appendingPathComponent("runnyctl").path
        // A link into a Runny.app that does NOT exist (the app was moved/deleted):
        // resolvingSymlinksInPath can't reach it, so the resolver must fall back to
        // the raw destination — otherwise a stale Runny link reads as foreign.
        let deadTarget = dir.appendingPathComponent("Runny.app/Contents/MacOS/runnyctl").path
        try fm.createSymbolicLink(atPath: link, withDestinationPath: deadTarget)
        let resolved = CLIInstallModel.resolvedLink(link)
        XCTAssertEqual(resolved, deadTarget)
        XCTAssertTrue(CLIInstall.isRunnyBundleCLI(resolved ?? ""),
                      "a dangling Runny.app link must classify as Runny-owned, not foreign")
    }

    func testRemoveScriptOnlyTouchesThisBundle() {
        let s = CLIInstallModel.removeScript(target: "/Applications/Runny.app/Contents/MacOS/runnyctl")
        XCTAssertTrue(s.contains("with administrator privileges"))
        // Removes only a link pointing at THIS bundle (exact compare, not a Runny.app
        // glob that would match another copy's link); anything else exits 3. (The
        // `"$existing"` quotes are escaped by the AppleScript layer, so assert on the
        // single-quoted target + rm, which pass through verbatim.)
        XCTAssertTrue(s.contains("= '/Applications/Runny.app/Contents/MacOS/runnyctl' ]; then rm -f /usr/local/bin/runnyctl"))
        XCTAssertTrue(s.contains("exit 3"))
        XCTAssertFalse(s.contains("*/Runny.app/Contents/MacOS/runnyctl)"), "must not use the loose Runny.app glob")
    }
}
