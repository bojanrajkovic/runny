import XCTest

@testable import Runny

import RunnyV1

@MainActor
final class CycleHistoryModelTests: XCTestCase {
    private func slot(_ name: String = "mac-1", cycleID: String = "cycle-a") -> Runny_V1_SlotStatus {
        var s = Runny_V1_SlotStatus()
        s.slot = name
        s.cycleID = cycleID
        return s
    }

    func testRefreshPopulatesCyclesFromClient() async {
        let store = DaemonStore()
        let fake = FakeRunnyClient()
        var record = Runny_V1_CycleRecord()
        record.cycleID = "cycle-a"
        fake.whyResult = .success([record])
        store.setClientForTest(fake)
        let model = CycleHistoryModel()

        model.refresh(slotName: "mac-1", cycleID: "cycle-a", store: store)

        let loaded = await poll { !model.cycles.isEmpty }
        XCTAssertTrue(loaded, "refresh must populate cycles from the client")
        XCTAssertEqual(model.cycles.first?.cycleID, "cycle-a")
        XCTAssertEqual(fake.whyCalls.first?.slot, "mac-1")
        XCTAssertFalse(model.loading, "loading must clear once the fetch resolves")
    }

    func testRefreshWithoutClientSetsUnreachableError() async {
        let store = DaemonStore()
        let model = CycleHistoryModel()

        model.refresh(slotName: "mac-1", cycleID: "cycle-a", store: store)

        XCTAssertEqual(model.loadError, "daemon unreachable")
        XCTAssertTrue(model.cycles.isEmpty)
    }

    func testRefreshOnFailureSetsLoadError() async {
        let store = DaemonStore()
        let fake = FakeRunnyClient()
        struct Boom: Error {}
        fake.whyResult = .failure(Boom())
        store.setClientForTest(fake)
        let model = CycleHistoryModel()

        model.refresh(slotName: "mac-1", cycleID: "cycle-a", store: store)

        let failed = await poll { model.loadError != nil }
        XCTAssertTrue(failed, "a why() failure must surface as loadError")
    }

    func testRefreshIfNeededSkipsARepeatFetchForTheSameCycle() async {
        let store = DaemonStore()
        let fake = FakeRunnyClient()
        fake.whyResult = .success([])
        store.setClientForTest(fake)
        let model = CycleHistoryModel()

        model.refreshIfNeeded(slot: slot(cycleID: "cycle-a"), store: store)
        let fetchedOnce = await poll { fake.whyCalls.count == 1 }
        XCTAssertTrue(fetchedOnce)

        // Same cycle again — must NOT re-fetch (keyed on fetchedForCycle, not
        // on emptiness: a slot with genuinely no completed cycles must not
        // re-fetch every call).
        model.refreshIfNeeded(slot: slot(cycleID: "cycle-a"), store: store)
        // No poll-for-true here on purpose: asserting a call count stays put
        // needs a settle window, not a first-true race.
        try? await Task.sleep(for: .milliseconds(50))
        XCTAssertEqual(fake.whyCalls.count, 1, "refreshIfNeeded must not re-fetch the same cycle")
    }

    func testRefreshIfNeededRefetchesOnANewCycle() async {
        let store = DaemonStore()
        let fake = FakeRunnyClient()
        fake.whyResult = .success([])
        store.setClientForTest(fake)
        let model = CycleHistoryModel()

        model.refreshIfNeeded(slot: slot(cycleID: "cycle-a"), store: store)
        _ = await poll { fake.whyCalls.count == 1 }

        model.refreshIfNeeded(slot: slot(cycleID: "cycle-b"), store: store)
        let refetched = await poll { fake.whyCalls.count == 2 }
        XCTAssertTrue(refetched, "a slot moving to a new cycle must trigger a re-fetch")
    }
}
