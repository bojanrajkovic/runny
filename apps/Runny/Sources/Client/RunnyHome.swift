import Foundation

/// Resolves the runny home directory and socket path.
///
/// runnyctl honors $RUNNY_HOME, but a Finder-launched app runs in the launchd
/// user session and never sees shell exports — so the app's override lives in
/// UserDefaults (Settings) instead. Default matches the daemon: ~/.runny.
enum RunnyHome {
    static let overrideDefaultsKey = "runnyHomeOverride"

    static var directory: URL {
        if let override = UserDefaults.standard.string(forKey: overrideDefaultsKey),
           !override.isEmpty
        {
            return URL(fileURLWithPath: (override as NSString).expandingTildeInPath)
        }
        return FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent(".runny")
    }

    static var socketPath: String {
        directory.appendingPathComponent("runnyd.sock").path
    }

    static var socketExists: Bool {
        FileManager.default.fileExists(atPath: socketPath)
    }

    /// Abbreviated for display ("~/.runny/runnyd.sock").
    static var displaySocketPath: String {
        let home = FileManager.default.homeDirectoryForCurrentUser.path
        let path = socketPath
        return path.hasPrefix(home) ? "~" + path.dropFirst(home.count) : path
    }
}
