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

    // Re-approval-after-reinstall: uninstalling then reinstalling re-derives the
    // approval state from status, so an agent macOS puts back in requiresApproval
    // on reinstall re-surfaces the Login Items deep-link CTA (Settings' "Open
    // Login Items…", the Start "Approve…" affordance) — it never gets stuck
    // installed or notInstalled across the cycle. End-to-end guarantee for the
    // approval-UX follow-up; the CTAs themselves gate on this state.
    func testReinstallReSurfacesRequiresApproval() async {
        let mock = MockRegistrar()
        mock.nextStatus = .enabled
        let c = AgentController(registrar: mock, eligibility: { .eligible })
        await c.install()
        XCTAssertEqual(c.installState, .installed)

        mock.nextStatus = .notRegistered // uninstall returns the agent to notRegistered
        await c.uninstall()
        XCTAssertEqual(c.installState, .notInstalled)

        mock.nextStatus = .requiresApproval // reinstall, but macOS now wants re-approval
        await c.install()
        XCTAssertEqual(
            c.installState, .requiresApproval,
            "a reinstalled agent pending re-approval must re-surface the Login Items CTA"
        )
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
        XCTAssertEqual(LaunchAgentStatus.startAffordance(state: .installed, daemonUnreachable: true, canonical: true), .start)
        XCTAssertEqual(LaunchAgentStatus.startAffordance(state: .installed, daemonUnreachable: false, canonical: true), .none)
        XCTAssertEqual(LaunchAgentStatus.startAffordance(state: .notInstalled, daemonUnreachable: true, canonical: true), .none)
    }

    func testStartHiddenForNonCanonicalAgent() {
        // A foreign or unverified agent: don't offer Start — it would kickstart the
        // foreign BundleProgram. Hidden until reconcile affirms canonical.
        XCTAssertEqual(
            LaunchAgentStatus.startAffordance(state: .installed, daemonUnreachable: true, canonical: false),
            .none
        )
        XCTAssertEqual(
            LaunchAgentStatus.startAffordance(state: .installed, daemonUnreachable: true, canonical: true),
            .start
        )
    }

    func testRequiresApprovalIsApprovalNeverStart() {
        // The dead-Start-button failure mode: a registered-but-unapproved agent must
        // route to the approval CTA, never a Start that kickstarts a job launchd won't run.
        XCTAssertEqual(LaunchAgentStatus.startAffordance(state: .requiresApproval, daemonUnreachable: true, canonical: true), .approval)
        XCTAssertEqual(LaunchAgentStatus.startAffordance(state: .requiresApproval, daemonUnreachable: false, canonical: true), .approval)
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

    func testReconcileDefaultsToNotCheckedNotCanonical() async {
        // An unchecked agent must NOT read as canonical (the default can't be .ok,
        // or Update would show for a foreign-but-unreconciled agent on the menu/main
        // surfaces where reconcile hasn't run).
        let c = AgentController(registrar: MockRegistrar())
        XCTAssertEqual(c.reconcileState, .notChecked)
        // A run produces a definitive verdict.
        let mock = MockRegistrar()
        mock.programResult = .program("/Applications/Runny.app/Contents/MacOS/runnyd")
        let c2 = AgentController(registrar: mock)
        await c2.runReconcile()
        XCTAssertEqual(c2.reconcileState, .ok)
    }

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

    func testReconcileResetsAfterUninstall() async {
        let mock = MockRegistrar()
        mock.programResult = .program("/foreign/Runny.app/Contents/MacOS/runnyd")
        let c = AgentController(registrar: mock)
        await c.runReconcile()
        XCTAssertEqual(c.reconcileState, .foreign(path: "/foreign/Runny.app/Contents/MacOS/runnyd"))
        // Uninstall must clear the stale verdict so a reinstall in the same session
        // re-checks and Start/Update aren't left hidden behind a dead .foreign.
        await c.uninstall()
        XCTAssertEqual(c.reconcileState, .notChecked)
    }

    func testNoteRecoveredResetsTerminalStartOutcome() async {
        let c = AgentController(registrar: MockRegistrar())
        await c.start(isConnected: { false }, within: .milliseconds(40), poll: .milliseconds(10))
        XCTAssertEqual(c.startOutcome, .didNotComeUp)
        // A later live connection proves recovery — clear the terminal outcome so the
        // next unrelated outage shows a fresh Start, not the stale "Try Again".
        c.noteRecovered()
        XCTAssertEqual(c.startOutcome, .idle)
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

    // MARK: - Reconcile repair

    func testCanRepairOnlyWhenForeignAndEligible() {
        XCTAssertTrue(AgentController.canRepair(reconcile: .foreign(path: "/x"), eligibility: .eligible))
        // A non-canonical bundle can't repair by re-registering — it would install
        // ANOTHER non-canonical agent; the surface shows move-to-/Applications guidance.
        XCTAssertFalse(AgentController.canRepair(reconcile: .foreign(path: "/x"), eligibility: .translocated))
        XCTAssertFalse(
            AgentController.canRepair(reconcile: .foreign(path: "/x"), eligibility: .notInApplications(path: "/y"))
        )
        // Nothing to repair unless the verdict is foreign.
        XCTAssertFalse(AgentController.canRepair(reconcile: .ok, eligibility: .eligible))
        XCTAssertFalse(AgentController.canRepair(reconcile: .notChecked, eligibility: .eligible))
        XCTAssertFalse(AgentController.canRepair(reconcile: .undetermined, eligibility: .eligible))
    }

    func testRepairReRegistersThenReReconcilesToSelfVerify() async {
        let mock = MockRegistrar()
        mock.programResult = .program("/foreign/Runny.app/Contents/MacOS/runnyd")
        mock.nextStatus = .enabled
        let c = AgentController(registrar: mock, eligibility: { .eligible })
        await c.runReconcile()
        XCTAssertEqual(c.reconcileState, .foreign(path: "/foreign/Runny.app/Contents/MacOS/runnyd"))

        // The re-register re-points the job; simulate the now-canonical program path.
        mock.programResult = .program("/Applications/Runny.app/Contents/MacOS/runnyd")
        await c.repair()
        XCTAssertEqual(mock.registerCalls, 1, "repair must re-register through the gate")
        XCTAssertEqual(c.reconcileState, .ok, "repair re-runs reconcile to confirm the re-point took")
        XCTAssertEqual(c.installState, .installed)
    }

    func testRepairThatDoesNotTakeStaysForeignNotFalseOk() async {
        // A foreign MANAGER still owning the label (the deferred detect-and-defer
        // case): re-register doesn't re-point, so the re-run reconcile must keep
        // showing foreign — never a false all-clear off the register call's return.
        let mock = MockRegistrar()
        mock.programResult = .program("/opt/homebrew/Cellar/runny/bin/runnyd")
        mock.nextStatus = .enabled
        let c = AgentController(registrar: mock, eligibility: { .eligible })
        await c.repair()
        XCTAssertEqual(c.reconcileState, .foreign(path: "/opt/homebrew/Cellar/runny/bin/runnyd"))
    }

    func testRepairDenyGateDoesNotReRegister() async {
        let mock = MockRegistrar()
        let c = AgentController(
            registrar: mock, spawnGate: { .deny(reason: "another manager owns it") }, eligibility: { .eligible }
        )
        await c.repair()
        XCTAssertEqual(mock.registerCalls, 0, "a denied gate must NOT re-register")
        XCTAssertNotNil(c.spawnRefusal)
    }

    func testRepairRegisterThrowIsLoud() async {
        let mock = MockRegistrar()
        mock.registerError = StubError()
        let c = AgentController(registrar: mock, eligibility: { .eligible })
        await c.repair()
        guard case .registrationFailed = c.installState else {
            return XCTFail("a register throw during repair must be loud, got \(c.installState)")
        }
    }
}
