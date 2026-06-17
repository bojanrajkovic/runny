import ServiceManagement
import XCTest

@testable import Runny

/// AgentController's decisions against a mock ServiceRegistrar — register/unregister
/// transitions, the requested-vs-done discipline (installState comes from status,
/// never the call return), the bootout-success-on-not-loaded rule, and the
/// spawn-chokepoint gate. No live SMAppService.
@MainActor
final class AgentControllerTests: XCTestCase {
    /// Records calls and returns scripted results — the seam that lets us assert
    /// *requested*, never fabricate *done*.
    final class MockRegistrar: ServiceRegistrar {
        var nextStatus: SMAppService.Status = .notRegistered
        var registerError: Error?
        var unregisterError: Error?
        var bootoutOutcome: BootoutOutcome = .notLoaded
        private(set) var registerCalls = 0
        private(set) var unregisterCalls = 0
        private(set) var bootoutCalls = 0

        func status() -> SMAppService.Status { nextStatus }
        func register() throws { registerCalls += 1; if let registerError { throw registerError } }
        func unregister() throws { unregisterCalls += 1; if let unregisterError { throw unregisterError } }
        func bootout() async -> BootoutOutcome { bootoutCalls += 1; return bootoutOutcome }
    }

    struct StubError: Error {}

    func testInstallDerivesInstalledFromStatusNotCallReturn() async {
        let mock = MockRegistrar()
        mock.nextStatus = .enabled // what status() reports after a successful register
        let c = AgentController(registrar: mock)
        await c.install()
        XCTAssertEqual(mock.registerCalls, 1)
        XCTAssertEqual(c.installState, .installed)
    }

    func testInstallSurfacesRequiresApprovalNotInstalled() async {
        let mock = MockRegistrar()
        mock.nextStatus = .requiresApproval
        let c = AgentController(registrar: mock)
        await c.install()
        XCTAssertEqual(c.installState, .requiresApproval)
    }

    func testRegisterThrowBecomesRegistrationFailedNeverSilentNotInstalled() async {
        let mock = MockRegistrar()
        mock.registerError = StubError()
        // Even if a later status() would say notRegistered, a throw is a loud failure.
        mock.nextStatus = .notRegistered
        let c = AgentController(registrar: mock)
        await c.install()
        guard case .registrationFailed = c.installState else {
            return XCTFail("a register() throw must surface registrationFailed, got \(c.installState)")
        }
    }

    func testUninstallUnregistersThenBootsOutAndConfirmsFromStatus() async {
        let mock = MockRegistrar()
        mock.nextStatus = .notRegistered // post-uninstall status
        mock.bootoutOutcome = .notLoaded // unregister already removed the job — success
        let c = AgentController(registrar: mock)
        await c.uninstall()
        XCTAssertEqual(mock.unregisterCalls, 1)
        XCTAssertEqual(mock.bootoutCalls, 1)
        XCTAssertEqual(c.installState, .notInstalled)
    }

    func testUninstallBootoutFailureIsLoud() async {
        let mock = MockRegistrar()
        mock.bootoutOutcome = .failed("launchd said no")
        let c = AgentController(registrar: mock)
        await c.uninstall()
        guard case .registrationFailed = c.installState else {
            return XCTFail("a real bootout failure must be loud, got \(c.installState)")
        }
    }

    func testDenyGateBlocksInstallWithoutCallingRegistrar() async {
        let mock = MockRegistrar()
        mock.nextStatus = .enabled
        let c = AgentController(registrar: mock, spawnGate: { .deny(reason: "another manager owns it") })
        await c.install()
        XCTAssertEqual(mock.registerCalls, 0, "a denied gate must NOT spawn")
        XCTAssertNotNil(c.spawnRefusal)
        XCTAssertEqual(c.installState, .notInstalled, "a denied install must not flip to installed")
    }

    func testEnableStartAtLoginAlsoRoutesThroughTheGate() async {
        let mock = MockRegistrar()
        let c = AgentController(registrar: mock, spawnGate: { .deny(reason: "deferred") })
        await c.enableStartAtLogin()
        XCTAssertEqual(mock.registerCalls, 0, "start-at-login enable must funnel through the same gate")
        XCTAssertNotNil(c.spawnRefusal)
    }

    func testEligibilityReflectsInjectedProvider() {
        let c = AgentController(registrar: MockRegistrar(), eligibility: { .translocated })
        XCTAssertEqual(c.eligibility, .translocated)
    }

    func testClassifyBootout() {
        XCTAssertEqual(AgentController.classifyBootout(exitCode: 0, stderr: ""), .removed)
        XCTAssertEqual(
            AgentController.classifyBootout(exitCode: 3, stderr: "Boot-out failed: 3: No such process"),
            .notLoaded
        )
        XCTAssertEqual(
            AgentController.classifyBootout(exitCode: 1, stderr: "No such process"),
            .notLoaded
        )
        guard case .failed = AgentController.classifyBootout(exitCode: 1, stderr: "permission denied") else {
            return XCTFail("a real launchctl error must classify as failed")
        }
    }
}
