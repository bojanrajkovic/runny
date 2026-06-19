import Foundation
import RunnyV1

/// Resolves the runny home directory and socket path.
///
/// The home LOCATION is deployment-resolved, never overridden (no environment
/// variable, no Settings field): the system-daemon home `/Library/Application
/// Support/runny` when it EXISTS, else the per-user `~/.runny`. This mirrors
/// Go's `home.ResolveClient` — selection by existence, not a liveness probe (that
/// stays `SocketProbe`'s job) — so a Finder-launched app and the daemon resolve
/// the same home and can never disagree about where the socket, credentials, and
/// artifacts live. The whole home resolves (not just the socket): `artifactURL`
/// reads `cycles/` from `directory`, so a system daemon's artifacts are only
/// app-readable when `directory` itself points at the system home.
enum RunnyHome {
    /// The system-daemon home. MUST stay in sync with Go's `home.SystemHomeDir`
    /// (internal/home/home.go) — Swift can't import the Go constant, so both
    /// sides hardcode it independently and `RunnyHomeResolutionTests` pins the
    /// literal as the cross-language drift guard (see the sharp edge in
    /// apps/Runny/CLAUDE.md). The privileged installer (the headless path)
    /// creates this dir owned by the service account with an inheriting ACL
    /// granting the operator; the per-user agent never uses it.
    static let systemHomeDir = "/Library/Application Support/runny"

    static var systemDirectory: URL { URL(fileURLWithPath: systemHomeDir, isDirectory: true) }
    static var perUserDirectory: URL {
        FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent(".runny")
    }

    /// The resolved home: the system home when present, else the per-user one.
    /// Computed per access (a stat), mirroring Go's per-call `ResolveClient` and
    /// the existing per-access socket check — the home can appear after launch
    /// (the installer runs while the app is up) and a cached value would lie.
    static var directory: URL {
        resolveDirectory(systemDirExists: FileManager.default.fileExists(atPath: systemHomeDir))
    }

    /// Pure resolution, split out so tests drive both branches without touching
    /// /Library. `directory` supplies the live existence check. Existence, not
    /// dir-ness, matches Go's `os.Stat` keying.
    static func resolveDirectory(systemDirExists: Bool) -> URL {
        systemDirExists ? systemDirectory : perUserDirectory
    }

    private static let socketName = "runnyd.sock"

    /// The socket inside a home — `<dir>/runnyd.sock`, the single join site
    /// (mirrors Go's `Dir.SocketPath`), so the dialed socket and the install
    /// gate's probed sockets can't drift apart.
    private static func socket(in dir: URL) -> String {
        dir.appendingPathComponent(socketName).path
    }

    /// The literal system / per-user socket paths, independent of resolution.
    /// `AgentController.liveSocketOccupied` probes BOTH so the install gate sees a
    /// live daemon at either path; dialing uses the resolved `socketPath`.
    static var systemSocketPath: String { socket(in: systemDirectory) }
    static var perUserSocketPath: String { socket(in: perUserDirectory) }

    /// Where the app dials the daemon: the socket inside the resolved home.
    static var socketPath: String { socket(in: directory) }

    static var socketExists: Bool {
        FileManager.default.fileExists(atPath: socketPath)
    }

    /// The directory the socket-appearance watcher arms on — the resolved home
    /// (the system dir for a system daemon, else the per-user home). The socket
    /// lives directly in the home, so this IS `directory`; deriving it from
    /// `socketPath` would re-stat and risk a string round-trip drifting off the
    /// one axis everything else resolves on.
    static var socketDirectory: URL { directory }

    /// Creates the PER-USER home if it is absent, owner-only (0700) to match the
    /// daemon. Defaults to `perUserDirectory`, NEVER the resolved `directory`:
    /// the system home is the privileged installer's to create (owned by the
    /// service account, with the operator ACL), so an operator-run app must not
    /// `mkdir` it. With dir-existence resolution the system home only wins when it
    /// already exists, so the watcher arms on it without any create — this method
    /// covers only the fresh per-user install, where the home does not exist until
    /// the first daemon run and the socket-appearance watcher's O_EVTONLY open
    /// would otherwise silently fail to arm (the app would wait out a full
    /// reconnect backoff instead of retrying the instant the socket appears).
    /// Idempotent: an existing directory is left untouched, including its
    /// permissions (the daemon owns the home's contents and may have tightened or
    /// relaxed it). Returns whether the directory exists afterward. The `url`
    /// parameter exists for tests.
    @discardableResult
    static func ensureDirectory(_ url: URL = perUserDirectory) -> Bool {
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

    /// Abbreviated for display ("~/.runny/runnyd.sock"); a system-home socket has
    /// no user-home prefix, so it shows its full path.
    static var displaySocketPath: String {
        let home = FileManager.default.homeDirectoryForCurrentUser.path
        let path = socketPath
        return path.hasPrefix(home) ? "~" + path.dropFirst(home.count) : path
    }

    /// On-disk location of a retained cycle artifact. The daemon stores
    /// artifacts under `cycles/<slot>/<RFC3339-started>-<cycleID>/<filename>`
    /// (the cycle store's `Dir`); this is the one place that mirrors that
    /// naming, so the eventual move to a daemon-provided path is a single edit.
    /// The app and daemon share a host (unix socket), so the file is local. Reads
    /// from the resolved `directory`, so a system daemon's artifacts resolve under
    /// its operator-ACL'd home.
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
