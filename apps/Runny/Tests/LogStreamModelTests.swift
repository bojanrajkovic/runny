import XCTest

@testable import Runny

import RunnyV1

@MainActor
final class LogStreamModelTests: XCTestCase {
    func testStartAppendsLinesFromClient() async {
        let store = DaemonStore()
        let fake = FakeRunnyClient()
        var line = Runny_V1_LogLine()
        line.level = "INFO"
        line.message = "hello"
        fake.logLines = [line]
        store.setClientForTest(fake)
        let model = LogStreamModel(slot: "mac-1", daemon: false)

        model.start(store: store)

        let received = await poll { !model.lines.isEmpty }
        XCTAssertTrue(received, "start must append lines the client streams")
        XCTAssertEqual(model.lines.first?.message, "hello")
        XCTAssertEqual(fake.streamLogsCalls.first?.slot, "mac-1")
        model.stop()
    }

    func testStopCancelsTheStreamHandle() async {
        let store = DaemonStore()
        let fake = FakeRunnyClient()
        store.setClientForTest(fake)
        let model = LogStreamModel(slot: nil, daemon: true)

        model.start(store: store)
        let dispatched = await poll { !fake.streamLogsCalls.isEmpty }
        XCTAssertTrue(dispatched)

        model.stop()

        let cancelled = await poll { fake.streamLogsCancelCount > 0 }
        XCTAssertTrue(cancelled, "stop must cancel the underlying RPC, not just the consuming task")
    }

    func testStartWithoutClientDoesNotDispatchUntilOneAppears() async {
        let store = DaemonStore()
        let fake = FakeRunnyClient()
        // No setClientForTest call — store.client stays nil.
        let model = LogStreamModel(slot: "mac-1", daemon: false)

        model.start(store: store)
        try? await Task.sleep(for: .milliseconds(50))
        XCTAssertTrue(model.lines.isEmpty)
        XCTAssertTrue(fake.streamLogsCalls.isEmpty, "must not dispatch to any client while store.client is nil")
        model.stop()
    }
}
