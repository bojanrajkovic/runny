import XCTest

@testable import Runny

@MainActor
final class AppUpdateMonitorTests: XCTestCase {
    private func update(_ version: String = "0.7.0") -> DaemonStore.AppUpdate {
        DaemonStore.AppUpdate(version: version, url: URL(string: "https://github.com/x/\(version)")!)
    }

    func testRunCheckSetsAvailableUpdateFromCheckFn() async {
        let monitor = AppUpdateMonitor()
        monitor.checkFn = { _ in self.update() }

        await monitor.runCheck()

        XCTAssertEqual(monitor.availableUpdate, update())
    }

    func testRunCheckLeavesAvailableUpdateNilWhenCheckFnReturnsNil() async {
        let monitor = AppUpdateMonitor()
        monitor.checkFn = { _ in nil }

        await monitor.runCheck()

        XCTAssertNil(monitor.availableUpdate)
    }

    func testRunCheckSkipsTheFetchWhenThePreferenceIsDisabled() async {
        UserDefaults.standard.set(false, forKey: Prefs.checkForAppUpdates)
        defer { UserDefaults.standard.removeObject(forKey: Prefs.checkForAppUpdates) }

        let monitor = AppUpdateMonitor()
        var called = false
        monitor.checkFn = { _ in called = true; return self.update() }

        await monitor.runCheck()

        XCTAssertFalse(called, "a disabled preference must skip the check entirely, not just discard its result")
        XCTAssertNil(monitor.availableUpdate)
    }

    func testShownUpdateHidesExactlyTheDismissedVersion() async {
        // availableUpdate is private(set) — driven through runCheck (its real
        // setter) rather than assigned directly, same as DaemonStore.apply
        // being the one way tests populate its snapshot-derived state.
        let monitor = AppUpdateMonitor()
        monitor.checkFn = { _ in self.update("0.7.0") }
        await monitor.runCheck()

        monitor.dismissedUpdate = update("0.7.0")
        XCTAssertNil(monitor.shownUpdate, "a dismissed release must not show again")

        monitor.dismissedUpdate = update("0.6.9")
        XCTAssertEqual(monitor.shownUpdate, update("0.7.0"), "dismissing a DIFFERENT version must not hide this one")
    }
}
