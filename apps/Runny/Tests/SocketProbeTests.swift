import Foundation
import XCTest

@testable import Runny

/// The bounded socket connect-probe: the dial-outcome → verdict mapping against a
/// fake dialer, the occupied policy (the actual #100 fix — a refused stale socket
/// no longer reads as occupied), and the real `POSIXSocketDialer` against actual
/// unix sockets, which validates the macOS connect() behavior the wedged-vs-stale
/// distinction depends on.
final class SocketProbeTests: XCTestCase {
    private struct FakeDialer: SocketDialer {
        let result: SocketDialResult
        func dial(path _: String, timeout _: Duration) async -> SocketDialResult { result }
    }

    // MARK: - Dial-outcome → verdict mapping

    func testConnectedIsListening() async {
        let r = await SocketProbe.probe(path: "/x", dialer: FakeDialer(result: .connected))
        XCTAssertEqual(r, .listening)
    }

    func testRefusedIsRefused() async {
        let r = await SocketProbe.probe(path: "/x", dialer: FakeDialer(result: .refused))
        XCTAssertEqual(r, .refused)
    }

    func testNoEntryIsAbsent() async {
        let r = await SocketProbe.probe(path: "/x", dialer: FakeDialer(result: .noEntry))
        XCTAssertEqual(r, .absent)
    }

    func testTimedOutIsIndeterminate() async {
        let r = await SocketProbe.probe(path: "/x", dialer: FakeDialer(result: .timedOut))
        XCTAssertEqual(r, .indeterminate)
    }

    func testDialFailedIsIndeterminate() async {
        let r = await SocketProbe.probe(path: "/x", dialer: FakeDialer(result: .dialFailed("boom")))
        XCTAssertEqual(r, .indeterminate)
    }

    // MARK: - The occupied policy (the #100 fix)

    func testOccupiedForListeningAndIndeterminate() {
        XCTAssertTrue(SocketProbe.occupied(.listening))
        // Ambiguous reads as occupied — a false "empty" would let the app stomp a
        // daemon it merely failed to probe.
        XCTAssertTrue(SocketProbe.occupied(.indeterminate))
    }

    func testStaleAndAbsentReadAsEmpty() {
        // THE FIX: an affirmatively-dead (refused) stale socket no longer reads as
        // occupied, so it stops blocking install. An absent file is empty too.
        XCTAssertFalse(SocketProbe.occupied(.refused))
        XCTAssertFalse(SocketProbe.occupied(.absent))
    }

    // MARK: - The real dialer against actual unix sockets

    func testRealDialerListeningWhenSocketAccepts() async {
        let path = Self.tmpSocketPath("listen")
        guard let listener = Self.makeListener(at: path) else { return XCTFail("could not bind a test listener") }
        defer { close(listener); unlink(path) }
        let r = await SocketProbe.probe(path: path)
        XCTAssertEqual(r, .listening)
    }

    func testRealDialerRefusedWhenSocketFileIsStale() async {
        // The wedged-vs-stale crux on the real OS: bind+listen, then close the
        // listener WITHOUT unlinking. The socket file persists (unix sockets aren't
        // auto-removed) but no process listens — exactly a crashed daemon's leftover
        // inode. connect() must return ECONNREFUSED → .refused.
        let path = Self.tmpSocketPath("stale")
        guard let listener = Self.makeListener(at: path) else { return XCTFail("could not bind a test listener") }
        close(listener)
        defer { unlink(path) }
        let r = await SocketProbe.probe(path: path)
        XCTAssertEqual(r, .refused)
    }

    func testRealDialerAbsentWhenNoFile() async {
        let path = Self.tmpSocketPath("absent")
        unlink(path)
        let r = await SocketProbe.probe(path: path)
        XCTAssertEqual(r, .absent)
    }

    // MARK: - Helpers

    /// A short `/tmp` path — `sun_path` caps at 104 bytes, and the per-process temp
    /// dir is already long enough to overflow it, so a bound socket needs a short path.
    private static func tmpSocketPath(_ tag: String) -> String { "/tmp/runny-sp-\(getpid())-\(tag).sock" }

    /// Bind+listen a unix socket at `path`, returning its fd (caller closes). Returns
    /// nil on any syscall failure so the test fails loudly rather than asserting on a
    /// half-set-up socket.
    private static func makeListener(at path: String) -> Int32? {
        unlink(path)
        let fd = socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else { return nil }
        var addr = sockaddr_un()
        addr.sun_family = sa_family_t(AF_UNIX)
        addr.sun_len = UInt8(MemoryLayout<sockaddr_un>.size)
        let capacity = MemoryLayout.size(ofValue: addr.sun_path)
        guard path.utf8.count < capacity else { close(fd); return nil }
        withUnsafeMutablePointer(to: &addr.sun_path.0) { dst in
            path.withCString { src in _ = strncpy(dst, src, capacity - 1) }
        }
        let bound = withUnsafePointer(to: &addr) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                Darwin.bind(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        guard bound == 0, listen(fd, 4) == 0 else { close(fd); return nil }
        return fd
    }
}
