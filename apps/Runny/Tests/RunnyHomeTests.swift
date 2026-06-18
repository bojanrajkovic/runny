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

/// The socket-appearance watcher opens the home with O_EVTONLY for instant
/// retry when the daemon's socket shows up. On a fresh install the home does
/// not exist until the first daemon run, so the watch silently fails to arm and
/// the app waits out its full reconnect backoff. `ensureDirectory` creates the
/// top-level home (0700, matching the daemon) so the watch arms on a clean
/// machine — without ever widening an existing home's permissions.
final class RunnyHomeEnsureDirectoryTests: XCTestCase {
    func testCreatesMissingDirectoryAsOwnerOnly() throws {
        let parent = FileManager.default.temporaryDirectory
            .appendingPathComponent("runny-ensure-\(UUID().uuidString)")
        let home = parent.appendingPathComponent(".runny")
        defer { try? FileManager.default.removeItem(at: parent) }

        XCTAssertFalse(FileManager.default.fileExists(atPath: home.path))
        XCTAssertTrue(RunnyHome.ensureDirectory(home))

        var isDir: ObjCBool = false
        XCTAssertTrue(FileManager.default.fileExists(atPath: home.path, isDirectory: &isDir))
        XCTAssertTrue(isDir.boolValue, "home must be a directory")

        let perms = try FileManager.default
            .attributesOfItem(atPath: home.path)[.posixPermissions] as? NSNumber
        XCTAssertEqual(perms?.intValue, 0o700, "a freshly-created home must be owner-only 0700")
    }

    func testPreservesExistingDirectoryPermissions() throws {
        let home = FileManager.default.temporaryDirectory
            .appendingPathComponent("runny-ensure-\(UUID().uuidString)")
        try FileManager.default.createDirectory(
            at: home, withIntermediateDirectories: true,
            attributes: [.posixPermissions: 0o755]
        )
        defer { try? FileManager.default.removeItem(at: home) }

        // Already present at 0755: ensureDirectory must leave it untouched, not
        // clamp it to 0700 or widen it.
        XCTAssertTrue(RunnyHome.ensureDirectory(home))
        let perms = try FileManager.default
            .attributesOfItem(atPath: home.path)[.posixPermissions] as? NSNumber
        XCTAssertEqual(perms?.intValue, 0o755, "an existing home's permissions must be preserved")
    }
}
