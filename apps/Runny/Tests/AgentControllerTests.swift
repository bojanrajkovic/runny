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
        /// When true, a successful `unregister()` does NOT flip status to
        /// `.notRegistered` — models launchd not yet processing the removal, so
        /// repair's bounded wait-for-removal is exercised.
        var unregisterDoesNotConfirm = false
        /// When true, a successful `register()` does NOT flip status to the registered
        /// state — models launchd accepting the re-register but not completing it, so
        /// repair's bounded wait-for-registration (`awaitRegistered`) is exercised.
        var registerDoesNotConfirm = false
        /// Set by `unregister()` (cleared by a successful `register()`) so `status()`
        /// reports the agent gone after a removal request — the requested-vs-done
        /// transition repair waits on.
        private var unregisteredPending = false
        var bootoutOutcome: BootoutOutcome = .notLoaded
        var kickstartError: Error?
        var programResult: AgentProgram = .notRegistered
        /// Consumed in order if non-empty (otherwise programResult), so a test can
        /// script a stale-then-fresh read across a coalesced reconcile.
        var programResults: [AgentProgram] = []
        /// Invoked once, inside the first agentProgramPath read, before it returns —
        /// the deterministic hook for "a concurrent trigger arrives mid-read".
        var onProgramPath: (() async -> Void)?
        private(set) var registerCalls = 0
        private(set) var unregisterCalls = 0
        private(set) var bootoutCalls = 0
        private(set) var kickstartCalls = 0
        private(set) var programCalls = 0

        func status() -> SMAppService.Status { unregisteredPending ? .notRegistered : nextStatus }
        func register() throws {
            registerCalls += 1
            if let registerError { throw registerError }
            // A re-register that took clears the removal; with registerDoesNotConfirm the
            // request is accepted but status keeps reading the removed state (launchd not
            // yet done), so awaitRegistered is exercised.
            if !registerDoesNotConfirm { unregisteredPending = false }
        }

        func unregister() throws {
            unregisterCalls += 1
            if let unregisterError { throw unregisterError }
            if !unregisterDoesNotConfirm { unregisteredPending = true }
        }

        func bootout() async -> BootoutOutcome { bootoutCalls += 1; return bootoutOutcome }
        func kickstart() async throws { kickstartCalls += 1; if let kickstartError { throw kickstartError } }
        func agentProgramPath() async -> AgentProgram {
            programCalls += 1
            if let hook = onProgramPath { onProgramPath = nil; await hook() }
            if !programResults.isEmpty { return programResults.removeFirst() }
            return programResult
        }
    }

    struct StubError: Error {}

    /// install() re-gathers ownership on success, so these install tests inject the
    /// ownership providers (probe/socket/plist) to stay hermetic — otherwise the
    /// post-install refresh would shell out to the real `launchctl`.
    private func hermeticInstall(_ mock: MockRegistrar, eligible: Bool = false) -> AgentController {
        AgentController(
            registrar: mock,
            eligibility: { eligible ? .eligible : .notInApplications(path: "/x") },
            probe: { _ in .notRegistered }, socketAnswers: { false }, manualPlistPersisted: { false }
        )
    }

    func testInstallDerivesInstalledFromStatusNotCallReturn() async {
        let mock = MockRegistrar()
        mock.nextStatus = .enabled // what status() reports after a successful register
        let c = hermeticInstall(mock)
        await c.install()
        XCTAssertEqual(mock.registerCalls, 1)
        XCTAssertEqual(c.installState, .installed)
    }

    func testInstallRefreshesOwnershipToSelfManaged() async {
        // Finding 4: the affordances gate on `ownership` (Start/Update need selfManaged,
        // the approval CTA awaitingApproval). install() must re-gather it on success, or
        // the just-installed agent stays the pre-install `.unmanaged` verdict until the
        // next app activation — suppressing the controls the install just enabled.
        let mock = MockRegistrar()
        mock.nextStatus = .enabled
        let c = hermeticInstall(mock)
        XCTAssertEqual(c.ownership, .indeterminate, "starts unchecked")
        await c.install()
        XCTAssertEqual(c.ownership, .selfManaged, "a successful install must publish the self-managed verdict immediately")
    }

    func testInstallSurfacesRequiresApprovalNotInstalled() async {
        let mock = MockRegistrar()
        mock.nextStatus = .requiresApproval
        let c = hermeticInstall(mock)
        await c.install()
        XCTAssertEqual(c.installState, .requiresApproval)
        // And the verdict follows to awaitingApproval, so the approval CTA shows at once.
        XCTAssertEqual(c.ownership, .awaitingApproval)
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
        let c = hermeticInstall(mock, eligible: true)
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

    // A pristine first launch reports `.notFound` from SMAppService (no record of
    // the agent yet, not the post-cycle `.notRegistered`). refresh() must consult
    // whether the plist is actually bundled: a real release (plist present) is
    // installable, a dev build (no plist) shows the honest no-daemon state — so a
    // first-time user is never stuck behind a disabled toggle + false "no daemon".
    func testRefreshTreatsNeverRegisteredBundledAgentAsInstallable() {
        let mock = MockRegistrar()
        mock.nextStatus = .notFound
        let release = AgentController(registrar: mock, bundledAgentPresent: { true })
        release.refresh()
        XCTAssertEqual(release.installState, .notInstalled, "a bundled-but-never-registered agent must be installable")
        let dev = AgentController(registrar: mock, bundledAgentPresent: { false })
        dev.refresh()
        XCTAssertEqual(dev.installState, .notFound)
    }

    // MARK: - Ownership gate

    func testGateForAllowsOwnAndUnmanagedDeniesEveryForeign() {
        XCTAssertEqual(AgentController.gateFor(.unmanaged), .allow)
        XCTAssertEqual(AgentController.gateFor(.selfManaged), .allow)
        for owner: DaemonOwnership in [.foreignBrew, .foreignManual, .foreground, .awaitingApproval, .indeterminate] {
            guard case .deny = AgentController.gateFor(owner) else {
                return XCTFail("\(owner) must deny spawning")
            }
        }
    }

    func testRefreshOwnershipClassifiesFromGatheredInputs() async {
        let mock = MockRegistrar()
        mock.nextStatus = .notRegistered // not ours
        let c = AgentController(
            registrar: mock,
            probe: { label in label == DaemonOwnership.brewLabel ? .registered : .notRegistered },
            socketAnswers: { false },
            homeIsCanonical: { true },
            manualPlistPersisted: { false }
        )
        await c.refreshOwnership()
        XCTAssertEqual(c.ownership, .foreignBrew, "a registered brew label with no self-agent is foreignBrew")
    }

    func testRefreshOwnershipSelfManagedWhenOurAgentEnabled() async {
        let mock = MockRegistrar()
        mock.nextStatus = .enabled // ours
        let c = AgentController(
            registrar: mock, probe: { _ in .notRegistered }, socketAnswers: { true }, homeIsCanonical: { true },
            manualPlistPersisted: { false }
        )
        await c.refreshOwnership()
        XCTAssertEqual(c.ownership, .selfManaged)
    }

    func testRefreshOwnershipForeignManualWhenDormantPlistPersists() async {
        // The round-6 wiring end-to-end: both probes silent, no socket, but the manual
        // installer's plist persists on disk — gatherOwnership must feed that signal to
        // classify so the verdict is foreignManual (a dormant owner), not unmanaged.
        let mock = MockRegistrar()
        mock.nextStatus = .notRegistered
        let c = AgentController(
            registrar: mock, probe: { _ in .notRegistered }, socketAnswers: { false }, homeIsCanonical: { true },
            manualPlistPersisted: { true }
        )
        await c.refreshOwnership()
        XCTAssertEqual(c.ownership, .foreignManual, "a persisted manual plist is a dormant owner, never unmanaged")
    }

    func testForeignOwnershipGateBlocksInstallWithoutRegistering() async {
        // The production wiring end-to-end: a foreign verdict → gateFor → .deny →
        // attemptSpawn refuses without ever calling register (no stomp).
        let mock = MockRegistrar()
        mock.nextStatus = .notRegistered
        let c = AgentController(registrar: mock, spawnGate: { AgentController.gateFor(.foreignBrew) })
        await c.install()
        XCTAssertEqual(mock.registerCalls, 0, "a foreign verdict must block install")
        XCTAssertNotNil(c.spawnRefusal)
        XCTAssertEqual(c.installState, .notInstalled)
    }

    func testDeniedInstallPublishesFreshOwnershipForTheBanner() async {
        // A foreign manager that appeared since the last refresh denies the pre-act
        // gate; install() must publish the fresh verdict so the observer banner
        // replaces the toggle, not leave a dead Install that silently no-ops.
        let mock = MockRegistrar()
        mock.nextStatus = .notRegistered
        let c = AgentController(
            registrar: mock,
            spawnGate: { AgentController.gateFor(.foreignBrew) },
            probe: { label in label == DaemonOwnership.brewLabel ? .registered : .notRegistered },
            socketAnswers: { false }, homeIsCanonical: { true }
        )
        await c.install()
        XCTAssertEqual(mock.registerCalls, 0)
        XCTAssertEqual(c.ownership, .foreignBrew, "a denied install must publish the fresh verdict so the banner appears")
    }

    func testObserverMessageNamesTheManagingChannel() {
        // The app's own domain — no observer banner.
        XCTAssertNil(AgentController.observerMessage(for: .unmanaged))
        XCTAssertNil(AgentController.observerMessage(for: .selfManaged))
        XCTAssertNil(AgentController.observerMessage(for: .awaitingApproval))

        let brew = AgentController.observerMessage(for: .foreignBrew)
        XCTAssertEqual(brew?.kind, .managedByHomebrew)
        XCTAssertEqual(brew?.message.contains("brew services restart runny"), true)
        // The recommended restart is destructive (no drain) — the guidance must say so,
        // since Runny can't drain a daemon it doesn't manage.
        XCTAssertEqual(brew?.message.contains("in-flight job"), true)

        let manual = AgentController.observerMessage(for: .foreignManual)
        XCTAssertEqual(manual?.kind, .managedManually)
        // bootout is immediate too — warn before recommending it.
        XCTAssertEqual(manual?.message.contains("no job is running"), true)
        // The checkout-free command, NOT tools/deploy/uninstall.sh — a host that
        // installed from a one-off .dmg no longer has the checkout. Red-tested by
        // swapping in the deploy-script string and confirming this fails.
        XCTAssertEqual(manual?.message.contains("launchctl bootout"), true)
        XCTAssertEqual(manual?.message.contains("uninstall.sh"), false)
        // Must also remove the persisted plist (install.sh writes it to
        // ~/Library/LaunchAgents); bootout alone leaves it to reload at next login. The
        // separator is `;` + `rm -f`, NOT `&&`: a dormant-plist owner has nothing to boot
        // out (bootout exits nonzero), so `&&` would skip the rm that is the whole point.
        XCTAssertEqual(manual?.message.contains("rm -f ~/Library/LaunchAgents/"), true)
        XCTAssertEqual(manual?.message.contains("&& rm"), false)

        XCTAssertEqual(AgentController.observerMessage(for: .foreground)?.kind, .foregroundDaemon)
        XCTAssertEqual(AgentController.observerMessage(for: .indeterminate)?.kind, .indeterminate)
    }

    // MARK: - Start affordance

    func testStartAffordanceOnlyWhenInstalledAndUnreachable() {
        XCTAssertEqual(
            LaunchAgentStatus.startAffordance(
                state: .installed, ownership: .selfManaged, daemonUnreachable: true, canonical: true
            ), .start
        )
        XCTAssertEqual(
            LaunchAgentStatus.startAffordance(
                state: .installed, ownership: .selfManaged, daemonUnreachable: false, canonical: true
            ), .none
        )
        XCTAssertEqual(
            LaunchAgentStatus.startAffordance(
                state: .notInstalled, ownership: .unmanaged, daemonUnreachable: true, canonical: true
            ), .none
        )
    }

    func testStartHiddenForNonCanonicalAgent() {
        // A foreign or unverified agent: don't offer Start — it would kickstart the
        // foreign BundleProgram. Hidden until reconcile affirms canonical.
        XCTAssertEqual(
            LaunchAgentStatus.startAffordance(
                state: .installed, ownership: .selfManaged, daemonUnreachable: true, canonical: false
            ), .none
        )
        XCTAssertEqual(
            LaunchAgentStatus.startAffordance(
                state: .installed, ownership: .selfManaged, daemonUnreachable: true, canonical: true
            ), .start
        )
    }

    func testStartHiddenWhenOwnershipIsNotSelfManaged() {
        // The Finding-2 gap: a stopped Homebrew service leaves OUR agent registered too
        // (installState .installed) while the daemon is unreachable, but ownership is
        // .foreignBrew. Start must hide — every kickstart would be rejected by the spawn
        // gate, so a rendered Start is a dead button that only loops "Try Again". The
        // observer banner carries the real guidance. Same for any non-selfManaged owner.
        for owner: DaemonOwnership in [.foreignBrew, .foreignManual, .foreground, .indeterminate, .unmanaged] {
            XCTAssertEqual(
                LaunchAgentStatus.startAffordance(
                    state: .installed, ownership: owner, daemonUnreachable: true, canonical: true
                ),
                .none,
                "Start must hide for \(owner) — only a selfManaged daemon is ours to kickstart"
            )
        }
    }

    func testRequiresApprovalIsApprovalNeverStart() {
        // The dead-Start-button failure mode: a registered-but-unapproved agent must
        // route to the approval CTA, never a Start that kickstarts a job launchd won't run.
        XCTAssertEqual(
            LaunchAgentStatus.startAffordance(
                state: .requiresApproval, ownership: .awaitingApproval, daemonUnreachable: true, canonical: true
            ), .approval
        )
        XCTAssertEqual(
            LaunchAgentStatus.startAffordance(
                state: .requiresApproval, ownership: .awaitingApproval, daemonUnreachable: false, canonical: true
            ), .approval
        )
    }

    func testApprovalHiddenWhenOwnershipNotAwaitingApproval() {
        // Approving launches the RunAtLoad agent from System Settings, OUTSIDE the spawn
        // gate, so the approval CTA is offered ONLY when ownership is awaitingApproval
        // (which itself defers whenever another owner is present). A .requiresApproval
        // self-status with a deferring verdict (a foreign owner, or an inconclusive probe)
        // suppresses the CTA — folding the round-3 view-level gate into the pure decision.
        for owner: DaemonOwnership in [.indeterminate, .foreignBrew, .foreignManual, .foreground] {
            XCTAssertEqual(
                LaunchAgentStatus.startAffordance(
                    state: .requiresApproval, ownership: owner, daemonUnreachable: true, canonical: true
                ),
                .none,
                "the approval CTA must hide for \(owner) — approving would create a competing manager"
            )
        }
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
        let c = AgentController(
            registrar: mock, spawnGate: { .deny(reason: "deferred") },
            probe: { _ in .notRegistered }, socketAnswers: { false }, manualPlistPersisted: { false }
        )
        await c.start(isConnected: { true })
        XCTAssertEqual(mock.kickstartCalls, 0, "a denied gate must NOT kickstart")
        guard case .refused = c.startOutcome else {
            return XCTFail("a denied start must be loud, got \(c.startOutcome)")
        }
    }

    func testDeniedStartPublishesFreshOwnershipForTheBanner() async {
        // Round-8 B: like install()'s denied path, a denied Start must publish the fresh
        // verdict the gate gathered — otherwise the stale .selfManaged keeps the Start row
        // visible with a Try Again that the gate re-denies on every press until the next
        // app activation. Publishing it lets the row give way to the observer banner.
        let mock = MockRegistrar()
        mock.nextStatus = .notRegistered
        let c = AgentController(
            registrar: mock,
            spawnGate: { AgentController.gateFor(.foreignBrew) },
            probe: { label in label == DaemonOwnership.brewLabel ? .registered : .notRegistered },
            socketAnswers: { false }, manualPlistPersisted: { false }
        )
        await c.start(isConnected: { false })
        XCTAssertEqual(mock.kickstartCalls, 0, "a denied gate must NOT kickstart")
        guard case .refused = c.startOutcome else {
            return XCTFail("a denied start must be loud, got \(c.startOutcome)")
        }
        XCTAssertEqual(
            c.ownership, .foreignBrew,
            "a denied start must publish the fresh verdict so the Start row gives way to the observer banner"
        )
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

        // The verified unregister→register re-points the job; simulate the
        // now-canonical program path the reconcile reads afterward.
        mock.programResult = .program("/Applications/Runny.app/Contents/MacOS/runnyd")
        await c.repair()
        XCTAssertEqual(mock.unregisterCalls, 1, "verified repair unregisters first, then re-registers")
        XCTAssertEqual(mock.registerCalls, 1, "repair re-registers through the gate")
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

    func testRepairDenyGateDoesNotReRegisterAndIsSurfaced() async {
        let mock = MockRegistrar()
        mock.nextStatus = .enabled // the existing (foreign) agent is still registered
        let c = AgentController(
            registrar: mock, spawnGate: { .deny(reason: "another manager owns it") }, eligibility: { .eligible },
            probe: { label in label == DaemonOwnership.brewLabel ? .registered : .notRegistered },
            socketAnswers: { false }, manualPlistPersisted: { false }
        )
        c.refresh() // installed
        await c.repair()
        XCTAssertEqual(mock.registerCalls, 0, "a denied gate must NOT re-register")
        XCTAssertEqual(mock.unregisterCalls, 0, "a denied repair must NOT unregister — the foreign agent stays intact")
        XCTAssertNotNil(c.repairError, "a denied repair must be surfaced in the row, not silently no-op")
        XCTAssertEqual(c.installState, .installed, "a denied repair must not change the install state")
        // Round-9: like install/start, a denied repair must publish the fresh verdict the
        // gate gathered, so the observer banner replaces the Repair UI instead of a button
        // the gate re-denies on every press until the next app activation.
        XCTAssertEqual(c.ownership, .foreignBrew, "a denied repair must publish the fresh foreign verdict")
    }

    func testRepairAbortsIfReRegisterDoesNotConfirm() async {
        // Symmetric to the unregister wait: register() returning means launchd ACCEPTED,
        // not completed. Repair must wait for the re-registered agent to actually appear
        // (installed / approval-pending) before declaring the re-point verified — otherwise
        // the immediate reconcile reads the still-absent agent as .notRegistered → .ok and
        // the warning vanishes while the registration is still converging. If it never
        // confirms within the bound, repair aborts loud rather than reporting a false re-point.
        let mock = MockRegistrar()
        mock.nextStatus = .enabled
        mock.registerDoesNotConfirm = true // launchd accepts the re-register but never completes it
        let c = AgentController(
            registrar: mock, eligibility: { .eligible },
            probe: { _ in .notRegistered }, socketAnswers: { false }, manualPlistPersisted: { false }
        )
        await c.repair(within: .milliseconds(60), poll: .milliseconds(10))
        XCTAssertEqual(mock.unregisterCalls, 1, "verified repair unregisters first")
        XCTAssertEqual(mock.registerCalls, 1, "then re-registers")
        XCTAssertNotNil(c.repairError, "an unconfirmed re-register must surface a loud failure, not a false-verified re-point")
    }

    func testRepairFailedReRegisterSurfacesErrorAndHonestState() async {
        // The verified repair unregisters, then re-registers. If the re-register
        // throws after the unregister took, the agent is genuinely gone — installState
        // must honestly reflect that (the toggle then offers reinstall), and the
        // failure is surfaced loudly rather than masquerading as still-installed.
        let mock = MockRegistrar()
        mock.registerError = StubError() // unregister succeeds, the re-register fails
        mock.nextStatus = .notRegistered // so the agent is now gone
        let c = AgentController(registrar: mock, eligibility: { .eligible })
        await c.repair()
        XCTAssertEqual(mock.unregisterCalls, 1, "verified repair unregisters first")
        XCTAssertEqual(c.installState, .notInstalled, "a failed re-register after unregister leaves the agent gone — honestly")
        XCTAssertNotNil(c.repairError, "a failed repair must surface its error")
    }

    func testFailedRepairClearsStaleForeignReconcile() async {
        // Finding 3: the repair's verified unregister→register replace. If the
        // re-register throws AFTER the unregister took, the agent is genuinely gone — so
        // the pre-repair .foreign reconcile verdict is now stale and must be re-resolved,
        // not left claiming an agent is still registered at the old path (which would
        // keep offering a Repair that just unregisters nothing).
        let mock = MockRegistrar()
        mock.programResult = .program("/foreign/Runny.app/Contents/MacOS/runnyd")
        mock.nextStatus = .enabled
        let c = AgentController(registrar: mock, eligibility: { .eligible })
        await c.runReconcile()
        XCTAssertEqual(c.reconcileState, .foreign(path: "/foreign/Runny.app/Contents/MacOS/runnyd"))
        // The unregister takes, then the re-register fails; launchctl now finds no agent.
        mock.registerError = StubError()
        mock.programResult = .notRegistered
        await c.repair()
        XCTAssertNotNil(c.repairError, "a failed repair must surface its error")
        XCTAssertEqual(c.installState, .notInstalled, "the agent is gone after a failed re-register")
        XCTAssertEqual(
            c.reconcileState, .ok,
            "the stale .foreign verdict must be re-resolved once the agent is gone — not left offering Repair"
        )
    }

    func testRepairAbortsIfUnregisterDoesNotConfirm() async {
        // An SMAppService call returning means the request was ACCEPTED, not that
        // launchd finished it. Repair must wait for status to confirm removal before
        // re-registering — otherwise the replace races a still-converging unregister
        // into an already-registered no-op. If removal never confirms within the
        // bound, repair aborts loud rather than re-registering over the old agent.
        let mock = MockRegistrar()
        mock.nextStatus = .enabled
        mock.unregisterDoesNotConfirm = true // launchd never processes the removal
        let c = AgentController(registrar: mock, eligibility: { .eligible })
        await c.repair(within: .milliseconds(60), poll: .milliseconds(10))
        XCTAssertEqual(mock.unregisterCalls, 1)
        XCTAssertEqual(mock.registerCalls, 0, "must NOT re-register before removal is confirmed")
        XCTAssertNotNil(c.repairError, "an unconfirmed removal must surface a loud repair failure")
    }

    // The repair self-verify must not be dropped by the reconcile in-flight guard:
    // if another surface's runReconcile() is mid-read when a fresh trigger arrives,
    // the in-flight run coalesces it and re-reads against the latest registration,
    // rather than publishing the stale pre-repair verdict.
    func testReconcileCoalescesAConcurrentTrigger() async {
        let mock = MockRegistrar()
        let c = AgentController(registrar: mock)
        mock.programResults = [
            .program("/foreign/Runny.app/Contents/MacOS/runnyd"),
            .program("/Applications/Runny.app/Contents/MacOS/runnyd"),
        ]
        // A second reconcile is triggered WHILE the first is mid-read — the exact
        // window that would otherwise drop the verification.
        mock.onProgramPath = { await c.runReconcile() }
        await c.runReconcile()
        XCTAssertEqual(mock.programCalls, 2, "a concurrent trigger must coalesce into a fresh read, not be dropped")
        XCTAssertEqual(c.reconcileState, .ok, "the final verdict must reflect the latest read, not the stale first one")
    }
}
