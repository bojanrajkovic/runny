import Foundation
import RunnyV1

/// Resolves the runny home directory and socket path.
///
/// The home is fixed at ~/.runny, derived from the current user — matching the
/// daemon's run-user derivation. There is no override (no environment variable,
/// no Settings field), so a Finder-launched app and the daemon can never
/// disagree about where the socket and credentials live.
enum RunnyHome {
    static var directory: URL {
        FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent(".runny")
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

    /// On-disk location of a retained cycle artifact. The daemon stores
    /// artifacts under `cycles/<slot>/<RFC3339-started>-<cycleID>/<filename>`
    /// (the cycle store's `Dir`); this is the one place that mirrors that
    /// naming, so the eventual move to a daemon-provided path is a single edit.
    /// The app and daemon share a host (unix socket), so the file is local.
    static func artifactURL(cycle: Runny_V1_CycleRecord, filename: String) -> URL {
        directory
            .appendingPathComponent("cycles")
            .appendingPathComponent(cycle.slot)
            .appendingPathComponent(cycleDirName(cycle))
            .appendingPathComponent(filename)
    }

    private static func cycleDirName(_ cycle: Runny_V1_CycleRecord) -> String {
        cycleDirFormatter.string(from: cycle.started.dateValue) + "-" + cycle.cycleID
    }

    /// Matches Go's `2006-01-02T15-04-05Z`: UTC, colons rendered as dashes,
    /// literal trailing Z.
    private static let cycleDirFormatter: DateFormatter = {
        let formatter = DateFormatter()
        formatter.dateFormat = "yyyy-MM-dd'T'HH-mm-ss'Z'"
        formatter.timeZone = TimeZone(identifier: "UTC")
        formatter.locale = Locale(identifier: "en_US_POSIX")
        return formatter
    }()
}
