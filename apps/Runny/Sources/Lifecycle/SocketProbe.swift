import Foundation

/// A bounded `connect()` probe of the daemon's unix socket — the precise signal a
/// file stat (`RunnyHome.socketExists`) cannot give. A present socket *file* means
/// only "a path exists": it could be a live listener, or the stale inode a crashed
/// hand-run daemon left behind. Telling them apart needs an actual connect:
///
///  - **refused** (`ECONNREFUSED`): the path exists but nobody is listening — a
///    stale inode. Affirmatively dead, so it must NOT block install. (A live but
///    *wedged* daemon still holds the socket in `listen()` state, so `connect()`
///    succeeds into the kernel backlog rather than being refused — wedged reads as
///    `listening`, not `refused`. This is the wedged-vs-stale distinction the file
///    stat conflated.)
///  - **listening**: `connect()` succeeded — a process holds the listening socket
///    (healthy, or wedged-but-still-accepting). Occupied: install stays blocked,
///    since installing a second manager over a live daemon is the stomp.
///  - **absent** (`ENOENT`): no file at the path. Nothing there.
///  - **indeterminate**: timed out or failed ambiguously — treated as occupied by
///    `occupied(_:)`, the safe direction (a false "empty" would let the app stomp a
///    daemon it merely failed to probe).
///
/// The connect is non-blocking and bounded by `poll()`, so a listener whose
/// backlog is saturated can't hang the probe — the no-unbounded-operations
/// invariant (ADR-0011) applied to the GUI's ownership gather, like `LaunchdProbe`.
enum SocketProbeResult: Equatable {
    case absent
    case refused
    case listening
    case indeterminate
}

/// The raw outcome of one bounded `connect()`, before policy mapping. Kept
/// separate from `SocketProbeResult` so the timeout/error → `indeterminate` policy
/// is unit-tested against a fake dialer — mirroring `CommandResult` →
/// `LaunchdProbeResult` in `LaunchdProbe`.
enum SocketDialResult: Equatable {
    case connected
    case refused
    case noEntry
    case timedOut
    case dialFailed(String)
}

enum SocketProbe {
    /// Hard wall-clock bound on one `connect()`. A local unix connect resolves in
    /// microseconds — success and `ECONNREFUSED` both return immediately — so this
    /// bound only ever fires on a saturated-backlog listener, where the non-blocking
    /// connect goes `EINPROGRESS` (or, on Darwin, `EAGAIN`) and the `poll()` wait
    /// engages. Healthy-magnitude × margin, not a budget sum.
    static let timeout: Duration = .seconds(2)

    /// Probe one socket path. The dialer seam is injectable so the result mapping is
    /// unit-tested without a real socket; the real `POSIXSocketDialer` is exercised
    /// by an integration test against actual unix sockets.
    static func probe(
        path: String = RunnyHome.socketPath, dialer: SocketDialer = POSIXSocketDialer()
    ) async -> SocketProbeResult {
        await classify(dialer.dial(path: path, timeout: timeout))
    }

    /// Map a raw dial outcome to a verdict. A clean refusal and a missing file are
    /// the two affirmatively-empty signals; a connect is `listening`; a timeout or
    /// any other failure is `indeterminate` — never a false "empty" off a probe that
    /// merely failed.
    static func classify(_ dial: SocketDialResult) -> SocketProbeResult {
        switch dial {
        case .connected: .listening
        case .refused: .refused
        case .noEntry: .absent
        case .timedOut, .dialFailed: .indeterminate
        }
    }

    /// Whether the socket axis should read as **occupied** (block install). Only the
    /// two affirmatively-empty outcomes — a refused stale inode or no file — read as
    /// empty; a live/wedged listener and any ambiguous probe read as occupied, the
    /// safe direction (a false "empty" is the stomp this project exists to kill).
    /// This is the boolean `DaemonOwnership.classify` consumes as `socketAnswers`.
    static func occupied(_ result: SocketProbeResult) -> Bool {
        switch result {
        case .listening, .indeterminate: true
        case .refused, .absent: false
        }
    }
}

/// The seam `SocketProbe` dials through, so the result mapping is unit-tested
/// against a fake while the real syscall discipline lives below it.
protocol SocketDialer: Sendable {
    /// Attempt a bounded `connect()` to the unix socket at `path`. Never hangs past
    /// `timeout`: the connect is non-blocking and the wait is `poll()`-bounded.
    func dial(path: String, timeout: Duration) async -> SocketDialResult
}

/// The real dialer: a non-blocking `connect()` to an `AF_UNIX` socket, bounded by
/// `poll()`. The only place raw socket syscalls live for the ownership gather.
struct POSIXSocketDialer: SocketDialer {
    func dial(path: String, timeout: Duration) async -> SocketDialResult {
        await withCheckedContinuation { (cont: CheckedContinuation<SocketDialResult, Never>) in
            // Hop off the caller's actor: the connect/poll are blocking syscalls
            // (bounded, but still syscalls), not work for the MainActor.
            DispatchQueue.global().async { cont.resume(returning: Self.dialSync(path: path, timeout: timeout)) }
        }
    }

    /// The synchronous connect, bounded by `poll()`. `defer { close(fd) }` guarantees
    /// the descriptor is released on every path — no FD leak even on an early error,
    /// mirroring `BoundedProcess`'s FD discipline.
    static func dialSync(path: String, timeout: Duration) -> SocketDialResult {
        let fd = socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else { return .dialFailed("socket(): errno \(errno)") }
        defer { close(fd) }

        // Non-blocking so connect() can't wedge on a saturated backlog; poll() bounds
        // the EINPROGRESS wait.
        let flags = fcntl(fd, F_GETFL, 0)
        guard flags >= 0, fcntl(fd, F_SETFL, flags | O_NONBLOCK) >= 0 else {
            return .dialFailed("fcntl(O_NONBLOCK): errno \(errno)")
        }

        var addr = sockaddr_un()
        addr.sun_family = sa_family_t(AF_UNIX)
        let capacity = MemoryLayout.size(ofValue: addr.sun_path) // 104 on Darwin
        guard path.utf8.count < capacity else { return .dialFailed("socket path too long for sun_path") }
        addr.sun_len = UInt8(MemoryLayout<sockaddr_un>.size)
        withUnsafeMutablePointer(to: &addr.sun_path.0) { dst in
            path.withCString { src in _ = strncpy(dst, src, capacity - 1) }
        }

        let rc = withUnsafePointer(to: &addr) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { sa in
                connect(fd, sa, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        if rc == 0 { return .connected }
        switch errno {
        // EINPROGRESS is the usual "completing asynchronously" code; on Darwin a
        // non-blocking AF_UNIX connect to a saturated-backlog listener can instead
        // return EAGAIN (== EWOULDBLOCK), which is *also* pollable for completion —
        // route both through the bounded poll so a live-but-busy daemon resolves to
        // `listening`/`timedOut` (occupied) rather than the `default` error path.
        case EINPROGRESS, EAGAIN: return Self.awaitConnect(fd: fd, timeout: timeout)
        case ECONNREFUSED: return .refused
        case ENOENT: return .noEntry
        default: return .dialFailed("connect(): errno \(errno)")
        }
    }

    /// Wait — bounded by `poll()` — for an in-progress non-blocking connect, then
    /// read `SO_ERROR` for the real outcome. `ECONNREFUSED` surfaced here is the same
    /// stale-inode signal as a synchronous refusal. `EINTR` retries against the
    /// REMAINING budget so a signal can't turn a still-resolving connect into a
    /// failure, and can't extend the wall-clock bound either.
    private static func awaitConnect(fd: Int32, timeout: Duration) -> SocketDialResult {
        let deadline = ContinuousClock.now.advanced(by: timeout)
        while true {
            let remaining = deadline - ContinuousClock.now
            if remaining <= .zero { return .timedOut }
            var pfd = pollfd(fd: fd, events: Int16(POLLOUT), revents: 0)
            let n = poll(&pfd, 1, Self.milliseconds(remaining))
            if n == 0 { return .timedOut }
            if n < 0 {
                if errno == EINTR { continue } // interrupted — re-poll with what's left
                return .dialFailed("poll(): errno \(errno)")
            }
            break
        }
        var soerr: Int32 = 0
        var len = socklen_t(MemoryLayout<Int32>.size)
        guard getsockopt(fd, SOL_SOCKET, SO_ERROR, &soerr, &len) >= 0 else {
            return .dialFailed("getsockopt(SO_ERROR): errno \(errno)")
        }
        // SO_ERROR after an async unix connect carries only success or refusal; ENOENT
        // is decided synchronously at connect() time, so it never arrives here.
        switch soerr {
        case 0: return .connected
        case ECONNREFUSED: return .refused
        default: return .dialFailed("connect: errno \(soerr)")
        }
    }

    /// `Duration` → `poll()` milliseconds, clamped to at least 1 for a positive
    /// duration so a sub-millisecond remainder never collapses to `poll(0)` (a
    /// non-blocking immediate-return), and saturating rather than overflowing.
    private static func milliseconds(_ duration: Duration) -> Int32 {
        let c = duration.components
        let ms = c.seconds * 1000 + c.attoseconds / 1_000_000_000_000_000
        return Int32(clamping: Swift.max(1, ms))
    }
}
