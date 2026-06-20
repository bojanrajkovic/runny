import XCTest

@testable import Runny

/// The pure helpers behind the shared admin-prompt broker: the trailing-paren-code
/// parse, the user-cancelled recovery, the shell/AppleScript escaping that keeps a
/// path with spaces or quotes from breaking out of the one privileged command, and
/// the translocation predicate. The osascript process execution itself is the thin
/// untested shell (live-machine only); everything that DECIDES is here. Both
/// privileged installers (the CLI symlink and the system daemon) share this, so a
/// quoting or exit-code fix can't land in one and miss the other.
final class PrivilegedBrokerTests: XCTestCase {
    // MARK: - osascript exit parsing

    func testTrailingParenCode() {
        XCTAssertEqual(PrivilegedBroker.trailingParenCode("execution error: ... (3)"), 3)
        XCTAssertEqual(PrivilegedBroker.trailingParenCode("User canceled. (-128)"), -128)
        // The LAST parenthesized code wins; an earlier "(3)" is ignored.
        XCTAssertEqual(PrivilegedBroker.trailingParenCode("path (3)/x failed (1)"), 1)
        XCTAssertNil(PrivilegedBroker.trailingParenCode("no trailing code here"))
        XCTAssertNil(PrivilegedBroker.trailingParenCode(""))
    }

    func testIsUserCancelled() {
        // A user-dismissed admin prompt is AppleScript error -128, by trailing code
        // OR the localized "User canceled" string.
        XCTAssertTrue(PrivilegedBroker.isUserCancelled(exitCode: 1, stderr: "execution error: User canceled. (-128)"))
        XCTAssertTrue(PrivilegedBroker.isUserCancelled(exitCode: 1, stderr: "something User Canceled something"))
        // Any other nonzero exit is NOT a cancellation — it's a real failure the
        // caller must surface, not swallow as a benign dismissal.
        XCTAssertFalse(PrivilegedBroker.isUserCancelled(exitCode: 1, stderr: "some other error (5)"))
        XCTAssertFalse(PrivilegedBroker.isUserCancelled(exitCode: 1, stderr: "the foreign guard fired (3)"))
        // A clean exit is never a cancellation, regardless of stderr noise.
        XCTAssertFalse(PrivilegedBroker.isUserCancelled(exitCode: 0, stderr: "User canceled. (-128)"))
    }

    // MARK: - translocation

    func testIsTranslocated() {
        XCTAssertTrue(PrivilegedBroker.isTranslocated("/private/var/folders/ab/cd/T/AppTranslocation/UUID/d/Runny.app"))
        XCTAssertTrue(PrivilegedBroker.isTranslocated("/private/var/folders/xy/z/Runny.app"))
        XCTAssertFalse(PrivilegedBroker.isTranslocated("/Applications/Runny.app"))
        XCTAssertFalse(PrivilegedBroker.isTranslocated("/Users/me/Applications/Runny.app"))
    }

    // MARK: - shell + AppleScript escaping (security-sensitive)

    func testShellSingleQuotePlain() {
        XCTAssertEqual(PrivilegedBroker.shellSingleQuote("/Applications/Runny.app"), "'/Applications/Runny.app'")
    }

    func testShellSingleQuoteWithSpace() {
        XCTAssertEqual(
            PrivilegedBroker.shellSingleQuote("/Users/me/My Apps/Runny.app/Contents/MacOS/runnyctl"),
            "'/Users/me/My Apps/Runny.app/Contents/MacOS/runnyctl'"
        )
    }

    func testShellSingleQuoteEscapesQuote() {
        // A single quote in the path must close-escape-reopen, never break out.
        XCTAssertEqual(PrivilegedBroker.shellSingleQuote("/a'b"), "'/a'\\''b'")
    }

    func testAppleScriptEscaping() {
        // Backslashes and double-quotes are escaped for the AppleScript string layer.
        XCTAssertEqual(
            PrivilegedBroker.appleScript(doShell: "echo \"hi\" \\ there"),
            "do shell script \"echo \\\"hi\\\" \\\\ there\" with administrator privileges"
        )
    }
}
