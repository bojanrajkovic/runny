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
        var kickstartError: Error?
        var programResult: AgentProgram = .notRegistered
        private(set) var registerCalls = 0
        private(set) var unregisterCalls = 0
        private(set) var bootoutCalls = 0
        private(set) var kickstartCalls = 0

        func status() -> SMAppService.Status { nextStatus }
        func register() throws { registerCalls += 1; if let registerError { throw registerError } }
        func unregister() throws { unregisterCalls += 1; if let unregisterError { throw unregisterError } }
        func bootout() async -> BootoutOutcome { bootoutCalls += 1; return bootoutOutcome }
        func kickstart() async throws { kickstartCalls += 1; if let kickstartError { throw kickstartError } }
        func agentProgramPath() async -> AgentProgram { programResult }
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

    func testEligibilityReflectsInjectedProvider() {
        let c = AgentController(registrar: MockRegistrar(), eligibility: { .translocated })
        XCTAssertEqual(c.eligibility, .translocated)
    }

    // MARK: - Start affordance

    func testStartAffordanceOnlyWhenInstalledAndUnreachable() {
        XCTAssertEqual(LaunchAgentStatus.startAffordance(state: .installed, daemonUnreachable: true), .start)
        XCTAssertEqual(LaunchAgentStatus.startAffordance(state: .installed, daemonUnreachable: false), .none)
        XCTAssertEqual(LaunchAgentStatus.startAffordance(state: .notInstalled, daemonUnreachable: true), .none)
    }

    func testRequiresApprovalIsApprovalNeverStart() {
        // The dead-Start-button failure mode: a registered-but-unapproved agent must
        // route to the approval CTA, never a Start that kickstarts a job launchd won't run.
        XCTAssertEqual(LaunchAgentStatus.startAffordance(state: .requiresApproval, daemonUnreachable: true), .approval)
        XCTAssertEqual(LaunchAgentStatus.startAffordance(state: .requiresApproval, daemonUnreachable: false), .approval)
    }

    func testStartConfirmsFromConnectionNotKickstartReturn() async {
        let mock = MockRegistrar()
        let c = AgentController(registrar: mock)
        await c.start(isConnected: { true }) // already connected → came up
        XCTAssertEqual(mock.kickstartCalls, 1)
        XCTAssertEqual(c.startOutcome, .cameUp)
    }

    func testStartThatNeverConnectsIsLoudNotSilent() async {
        let mock = MockRegistrar()
        let c = AgentController(registrar: mock)
        await c.start(isConnected: { false }, within: .milliseconds(60), poll: .milliseconds(10))
        XCTAssertEqual(c.startOutcome, .didNotComeUp)
    }

    func testStartGateDenyDoesNotKickstart() async {
        let mock = MockRegistrar()
        let c = AgentController(registrar: mock, spawnGate: { .deny(reason: "deferred") })
        await c.start(isConnected: { true })
        XCTAssertEqual(mock.kickstartCalls, 0, "a denied gate must NOT kickstart")
        guard case .refused = c.startOutcome else {
            return XCTFail("a denied start must be loud, got \(c.startOutcome)")
        }
    }

    func testStartKickstartFailureIsRefusedNotInstalledStateChange() async {
        let mock = MockRegistrar()
        mock.nextStatus = .enabled
        mock.kickstartError = StubError()
        let c = AgentController(registrar: mock)
        c.refresh() // installed
        await c.start(isConnected: { false }, within: .milliseconds(60), poll: .milliseconds(10))
        guard case .refused = c.startOutcome else {
            return XCTFail("a kickstart failure must surface refused, got \(c.startOutcome)")
        }
        // A failed START must not flip the INSTALL state to failed — the agent is still installed.
        XCTAssertEqual(c.installState, .installed)
    }

    // MARK: - Reconcile

    func testReconcileComparesAgainstCanonicalNotRunningBundle() async {
        let mock = MockRegistrar()
        // A /Applications agent is good even when observed from a translocated launch.
        mock.programResult = .program("/Applications/Runny.app/Contents/MacOS/runnyd")
        let c = AgentController(registrar: mock)
        await c.runReconcile()
        XCTAssertEqual(c.reconcileState, .ok)
    }

    func testReconcileFlagsForeignProgramPath() async {
        let mock = MockRegistrar()
        mock.programResult = .program("/Users/someone/Downloads/Runny.app/Contents/MacOS/runnyd")
        let c = AgentController(registrar: mock)
        await c.runReconcile()
        XCTAssertEqual(c.reconcileState, .foreign(path: "/Users/someone/Downloads/Runny.app/Contents/MacOS/runnyd"))
    }

    func testReconcileNotRegisteredIsOkUndeterminedIsLoud() async {
        let mock = MockRegistrar()
        mock.programResult = .notRegistered
        let c = AgentController(registrar: mock)
        await c.runReconcile()
        XCTAssertEqual(c.reconcileState, .ok)

        mock.programResult = .undetermined
        await c.runReconcile()
        XCTAssertEqual(c.reconcileState, .undetermined)
    }

    func testParseLaunchctlProgram() {
        let output = """
        com.coderinserepeat.runnyd = {
        \tactive count = 1
        \tprogram = /Applications/Runny.app/Contents/MacOS/runnyd
        \targuments = {
        }
        """
        XCTAssertEqual(
            AgentController.parseLaunchctlProgram(output),
            "/Applications/Runny.app/Contents/MacOS/runnyd"
        )
        // No program line (old/unparseable format) → nil, which reconciles to
        // undetermined rather than a false foreign.
        XCTAssertNil(AgentController.parseLaunchctlProgram("state = running\npid = 42"))
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
