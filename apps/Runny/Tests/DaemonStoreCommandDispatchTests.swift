import XCTest

@testable import Runny

import RunnyV1

/// Command dispatch (pause/resume/recycle/doctor/reload) against a
/// FakeRunnyClient, injected via `setClientForTest` — bypassing
/// `start()`'s connection-establishment flow (and its home-directory side
/// effects) entirely. `run()`'s RPC dispatch is a fire-and-forget internal
/// Task, not itself awaitable without changing every caller's signature
/// (SlotDetailView etc.), so these poll for the fake's recorded call rather
/// than awaiting the dispatching method directly.
@MainActor
final class DaemonStoreCommandDispatchTests: XCTestCase {
    private func slot(_ name: String = "mac-1", cycleID: String = "cycle-a") -> Runny_V1_SlotStatus {
        var s = Runny_V1_SlotStatus()
        s.slot = name
        s.cycleID = cycleID
        return s
    }

    /// Polls `condition` until it's true or `timeout` elapses — `run()`'s
    /// dispatch Task runs on the same MainActor executor as the poll loop, so
    /// a fake with no artificial delay resolves within a scheduling tick, not
    /// the full timeout; this only guards against actually hanging.
    private func poll(timeout: TimeInterval = 2, _ condition: () -> Bool) async -> Bool {
        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            if condition() { return true }
            try? await Task.sleep(for: .milliseconds(10))
        }
        return condition()
    }

    func testPauseSlotDispatchesToClient() async {
        let store = DaemonStore()
        let fake = FakeRunnyClient()
        store.setClientForTest(fake)

        store.pauseSlot(slot("mac-1"))

        let dispatched = await poll { !fake.pauseCalls.isEmpty }
        XCTAssertTrue(dispatched, "pauseSlot must dispatch to the client")
        XCTAssertEqual(fake.pauseCalls.first?.slot, "mac-1")
    }

    func testResumeSlotDispatchesToClient() async {
        let store = DaemonStore()
        let fake = FakeRunnyClient()
        store.setClientForTest(fake)

        store.resumeSlot(slot("mac-1"))

        let dispatched = await poll { !fake.resumeCalls.isEmpty }
        XCTAssertTrue(dispatched, "resumeSlot must dispatch to the client")
        XCTAssertEqual(fake.resumeCalls.first?.slot, "mac-1")
    }

    func testRequestRecycleOfSafeStateDispatchesAtOnceWithoutConsent() async {
        // Default state is neither JOB nor DEBUG, so recycleNeedsConsent is
        // false and requestRecycle must recycle immediately, not stage
        // recycleConfirm for a view to present.
        let store = DaemonStore()
        let fake = FakeRunnyClient()
        store.setClientForTest(fake)

        store.requestRecycle(slot("mac-1"))

        XCTAssertNil(store.recycleConfirm, "a safe-state recycle must not stage a confirmation")
        let dispatched = await poll { !fake.recycleCalls.isEmpty }
        XCTAssertTrue(dispatched, "requestRecycle of a safe state must dispatch to the client")
        XCTAssertEqual(fake.recycleCalls.first?.slot, "mac-1")
        XCTAssertEqual(fake.recycleCalls.first?.cancelRunningJob, false)
    }

    func testCommandWithoutClientFailsFastWithoutDispatching() async {
        // No setClientForTest call — client stays nil, exactly the
        // unreachable state every command guards against.
        let store = DaemonStore()
        let fake = FakeRunnyClient()

        store.pauseSlot(slot("mac-1"))

        XCTAssertEqual(store.commandError, "daemon unreachable — pause not sent")
        XCTAssertTrue(fake.pauseCalls.isEmpty, "a client-less command must never reach any client, fake or real")
    }

    func testRunDoctorPopulatesChecksFromClient() async {
        let store = DaemonStore()
        let fake = FakeRunnyClient()
        var check = Runny_V1_DoctorCheck()
        check.name = "socket"
        check.ok = true
        fake.doctorResult = .success([check])
        store.setClientForTest(fake)

        store.runDoctor()

        let ran = await poll { store.doctorChecks != nil }
        XCTAssertTrue(ran, "runDoctor must populate doctorChecks from the client")
        XCTAssertEqual(store.doctorChecks?.first?.name, "socket")
        XCTAssertEqual(fake.doctorCallCount, 1)
    }

    func testPerformReloadDispatchesToClientAndClearsInFlight() async {
        let store = DaemonStore()
        let fake = FakeRunnyClient()
        store.setClientForTest(fake)

        store.performReload()

        let dispatched = await poll { !fake.reloadCalls.isEmpty }
        XCTAssertTrue(dispatched, "performReload must dispatch to the client")
        let settled = await poll { !store.reloadInFlight }
        XCTAssertTrue(settled, "reloadInFlight must clear once the RPC resolves")
    }
}
