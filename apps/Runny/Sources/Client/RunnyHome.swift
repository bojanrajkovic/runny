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

    /// The shared system-daemon control socket directory. MUST stay in sync with
    /// Go's `home.SharedSocketDir` (internal/home/socket.go) — Swift can't import
    /// the Go constant, so both sides hardcode it independently (see the sharp
    /// edge in apps/Runny/CLAUDE.md). The privileged installer (the headless
    /// path) creates this dir owned by the service account with an ACL granting
    /// the operator; the per-user agent never uses it.
    static let sharedSocketDir = "/Library/Application Support/runny"
    static var sharedSocketPath: String {
        (sharedSocketDir as NSString).appendingPathComponent("runnyd.sock")
    }

    static var perUserSocketPath: String {
        directory.appendingPathComponent("runnyd.sock").path
    }

    /// Where the app dials the daemon. Mirrors Go's `home.ClientSocketPath`:
    /// prefer the shared system socket when it EXISTS (path selection, not
    /// liveness — that stays `SocketProbe`'s job), else the per-user socket, so
    /// the app reaches a system daemon with no configuration.
    static var socketPath: String {
        resolveSocketPath(sharedExists: FileManager.default.fileExists(atPath: sharedSocketPath))
    }

    /// Pure resolution, split out so tests can drive both branches without
    /// touching /Library. `socketPath` supplies the live existence check.
    static func resolveSocketPath(sharedExists: Bool) -> String {
        sharedExists ? sharedSocketPath : perUserSocketPath
    }

    static var socketExists: Bool {
        FileManager.default.fileExists(atPath: socketPath)
    }

    /// The directory the socket-appearance watcher arms on — the directory of the
    /// resolved socket (the shared dir for a system daemon, else the per-user
    /// home).
    static var socketDirectory: URL {
        URL(fileURLWithPath: socketPath).deletingLastPathComponent()
    }

    /// Creates the home directory if it is absent, owner-only (0700) to match
    /// the daemon. The socket-appearance watcher opens this directory with
    /// O_EVTONLY; on a fresh install — before the first daemon run — it does not
    /// exist, so the watch would silently fail to arm and the app would wait out
    /// a full reconnect backoff instead of retrying the instant the socket
    /// appears. Idempotent: an existing directory is left untouched, including
    /// its permissions (the daemon owns the home's contents and may have
    /// tightened or relaxed it). Returns whether the home exists as a directory
    /// afterward. The `url` parameter exists for tests; production passes the
    /// fixed `directory`.
    @discardableResult
    static func ensureDirectory(_ url: URL = directory) -> Bool {
        let fm = FileManager.default
        var isDir: ObjCBool = false
        if fm.fileExists(atPath: url.path, isDirectory: &isDir) {
            return isDir.boolValue
        }
        do {
            try fm.createDirectory(
                at: url, withIntermediateDirectories: true,
                attributes: [.posixPermissions: 0o700]
            )
            return true
        } catch {
            return false
        }
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
