import XCTest

@testable import Runny

/// The app home is fixed at ~/.runny with no override. The Settings key that
/// used to steer it ("runnyHomeOverride") is gone — setting it must no longer
/// move the home, so the socket still resolves under ~/.runny. The Swift
/// analogue of the daemon's env-ignored `Resolve` test: a stale override left in
/// a user's defaults plist is inert, not a path that splits the app from the
/// daemon.
final class RunnyHomeTests: XCTestCase {
    func testRemovedOverrideKeyNoLongerSteersTheHome() {
        let key = "runnyHomeOverride"
        UserDefaults.standard.set("/tmp/junk-home", forKey: key)
        defer { UserDefaults.standard.removeObject(forKey: key) }

        XCTAssertTrue(
            RunnyHome.socketPath.hasSuffix("/.runny/runnyd.sock"),
            "a set override key must not steer the home; got \(RunnyHome.socketPath)"
        )
        XCTAssertEqual(RunnyHome.displaySocketPath, "~/.runny/runnyd.sock")
    }
}
