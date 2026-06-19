import XCTest

@testable import Runny

/// The pure ownership verdict: `classify` over launchd-probe + SMAppService-self
/// inputs, with indeterminate-dominant precedence so "not sure who owns this"
/// can never read as "install a second manager" or "kill a hand-run daemon".
/// Pure → every branch is pinned without launchd or a live SMAppService.
final class DaemonOwnershipTests: XCTestCase {
    /// A clear, install-allowed baseline; each test overrides only what it exercises.
    private func inputs(
        homeIsCanonical: Bool = true,
        selfState: LaunchAgentStatus.State = .notInstalled,
        brewProbe: LaunchdProbeResult = .notRegistered,
        canonicalProbe: LaunchdProbeResult = .notRegistered,
        systemProbe: LaunchdProbeResult = .notRegistered,
        socketAnswers: Bool = false,
        manualPlistPersisted: Bool = false
    ) -> DaemonOwnershipInputs {
        DaemonOwnershipInputs(
            homeIsCanonical: homeIsCanonical, selfState: selfState,
            brewProbe: brewProbe, canonicalProbe: canonicalProbe, systemProbe: systemProbe,
            socketAnswers: socketAnswers, manualPlistPersisted: manualPlistPersisted
        )
    }

    func testUnmanagedWhenNothingOwnsItAndSocketSilent() {
        XCTAssertEqual(DaemonOwnership.classify(inputs()), .unmanaged)
    }

    func testSelfManagedWhenOurAgentEnabled() {
        // .installed == SMAppService .enabled == ours (C1). selfManaged even though
        // OUR agent makes the canonical label read registered.
        XCTAssertEqual(
            DaemonOwnership.classify(inputs(selfState: .installed, canonicalProbe: .registered)),
            .selfManaged
        )
    }

    func testAwaitingApprovalWhenOurAgentUnapproved() {
        XCTAssertEqual(DaemonOwnership.classify(inputs(selfState: .requiresApproval)), .awaitingApproval)
    }

    func testForeignBrewWhenBrewLabelRegistered() {
        XCTAssertEqual(DaemonOwnership.classify(inputs(brewProbe: .registered)), .foreignBrew)
    }

    func testForeignManualWhenCanonicalRegisteredButNotOurs() {
        // canonical label registered + self NOT enabled ⇒ a manual installer owns it.
        XCTAssertEqual(
            DaemonOwnership.classify(inputs(selfState: .notInstalled, canonicalProbe: .registered)),
            .foreignManual
        )
    }

    func testForegroundWhenSocketAnswersWithNoAgent() {
        XCTAssertEqual(DaemonOwnership.classify(inputs(socketAnswers: true)), .foreground)
    }

    func testForeignSystemWhenSystemLabelRegistered() {
        // A runnyd in the system/ domain — the headless non-root daemon — is a foreign
        // owner the app observes (over the shared socket) and never installs over.
        XCTAssertEqual(DaemonOwnership.classify(inputs(systemProbe: .registered)), .foreignSystem)
    }

    func testForeignSystemSurfacesOverSelfAndForeground() {
        // Like brew, a system daemon surfaces ahead of self (system daemon + our own
        // agent is a real two-manager conflict)...
        XCTAssertEqual(
            DaemonOwnership.classify(inputs(selfState: .installed, systemProbe: .registered)),
            .foreignSystem
        )
        // ...and ahead of the foreground branch, so a system daemon answering the
        // shared socket is named, not mislabeled a hand-run daemon.
        XCTAssertEqual(
            DaemonOwnership.classify(inputs(systemProbe: .registered, socketAnswers: true)),
            .foreignSystem
        )
    }

    func testIndeterminateSystemProbeDefersNotUnmanaged() {
        // P1 (Codex #114): a wedged/ambiguous system/ probe must fail CLOSED — it might be
        // a live system daemon — so it defers, never falls through to unmanaged (which
        // would install a competing per-user agent over it). Mirrors the brew/canonical
        // indeterminate guard.
        XCTAssertEqual(DaemonOwnership.classify(inputs(systemProbe: .indeterminate)), .indeterminate)
    }

    func testIndeterminateSystemProbeDefersApproval() {
        // The same wedge must block the approval all-clear: approving our agent over a
        // possibly-present system daemon would create a competing manager.
        XCTAssertEqual(
            DaemonOwnership.classify(inputs(selfState: .requiresApproval, systemProbe: .indeterminate)),
            .indeterminate
        )
    }

    func testSystemProbeIndeterminateWithSelfInstalledStaysSelfManaged() {
        // The boundary: a wedged system probe does NOT override authoritative self-identity
        // (an .installed agent is ours), exactly as for the brew/canonical probes — we never
        // defer managing our OWN daemon over an inconclusive probe.
        XCTAssertEqual(
            DaemonOwnership.classify(inputs(selfState: .installed, systemProbe: .indeterminate)),
            .selfManaged
        )
    }

    // MARK: - Indeterminate dominates (the regression guards)

    func testNonCanonicalHomeIsIndeterminateRegardlessOfEverythingElse() {
        // B5 guard (defense-in-depth): even with a positive registration, a
        // non-canonical home means socket and label axes describe different homes.
        XCTAssertEqual(
            DaemonOwnership.classify(inputs(homeIsCanonical: false, brewProbe: .registered)),
            .indeterminate
        )
    }

    func testDeterminateForeignSurfacesOverAnInconclusiveProbe() {
        // A *registered* foreign owner is strictly more informative than
        // "indeterminate" (both deny), so a positive registration surfaces even when
        // the OTHER probe wedged — it must not be hidden behind a defer.
        XCTAssertEqual(
            DaemonOwnership.classify(inputs(brewProbe: .registered, canonicalProbe: .indeterminate)),
            .foreignBrew
        )
        XCTAssertEqual(
            DaemonOwnership.classify(inputs(brewProbe: .indeterminate, canonicalProbe: .registered)),
            .foreignManual
        )
    }

    func testInconclusiveProbeDominatesThePermissiveVerdicts() {
        // The real invariant: an inconclusive probe with NO determinate owner must
        // never fall through to unmanaged (install) or foreground (stop the daemon).
        XCTAssertEqual(DaemonOwnership.classify(inputs(brewProbe: .indeterminate)), .indeterminate)
        XCTAssertEqual(
            DaemonOwnership.classify(inputs(canonicalProbe: .indeterminate, socketAnswers: true)),
            .indeterminate
        )
    }

    func testRequiresApprovalDefersWhenAnythingElseOwnsTheDaemon() {
        // Approving launches the RunAtLoad agent OUTSIDE the spawn gate (a System
        // Settings action), so awaitingApproval is safe only when nothing else owns
        // the daemon. A registered canonical label (ambiguous: ours-pending vs a
        // foreign manual one) OR an occupied socket (a foreground daemon) means
        // approving would create a competing manager — defer.
        XCTAssertEqual(
            DaemonOwnership.classify(inputs(selfState: .requiresApproval, canonicalProbe: .registered)),
            .indeterminate
        )
        XCTAssertEqual(
            DaemonOwnership.classify(inputs(selfState: .requiresApproval, socketAnswers: true)),
            .indeterminate
        )
        // An INCONCLUSIVE brew/canonical probe also defers — approval is safe only when
        // a foreign owner is definitively ruled out, not merely unprobed.
        XCTAssertEqual(
            DaemonOwnership.classify(inputs(selfState: .requiresApproval, brewProbe: .indeterminate)),
            .indeterminate
        )
        XCTAssertEqual(
            DaemonOwnership.classify(inputs(selfState: .requiresApproval, canonicalProbe: .indeterminate)),
            .indeterminate
        )
        // Only definitively all-clear stays the approval CTA.
        XCTAssertEqual(
            DaemonOwnership.classify(inputs(selfState: .requiresApproval)),
            .awaitingApproval
        )
    }

    func testUnknownSelfStatusFailsClosed() {
        // An unrecognized future SMAppService status maps to .registrationFailed — a
        // determination FAILURE, not a confirmed not-installed. Defer (never install)
        // rather than treat unknown registration state as install permission.
        XCTAssertEqual(
            DaemonOwnership.classify(inputs(selfState: .registrationFailed(reason: "x"))),
            .indeterminate
        )
    }

    func testPositiveBrewOverridesSelfOwnership() {
        // When our agent is enabled AND a brew service is registered, that is a real
        // two-manager conflict — surface the brew daemon, never hide it as selfManaged.
        // (Self identity overrides an inconclusive probe, but not an affirmative
        // foreign registration.)
        XCTAssertEqual(
            DaemonOwnership.classify(inputs(selfState: .installed, brewProbe: .registered)),
            .foreignBrew
        )
    }

    func testSelfIdentityDominatesAWedgedProbe() {
        // A transient launchctl probe wedge must NOT override the authoritative
        // SMAppService self-status: an installed (.enabled) agent is ours even if a
        // foreign-label probe times out, so we never defer managing our own daemon.
        // Note this is .installed ONLY: .enabled is authoritative enough to override a
        // wedge, but .requiresApproval is NOT (the pending agent isn't running, so it
        // can't attest to the loaded label) — that case defers, asserted in
        // testRequiresApprovalDefersWhenAnythingElseOwnsTheDaemon.
        XCTAssertEqual(
            DaemonOwnership.classify(inputs(selfState: .installed, brewProbe: .indeterminate)),
            .selfManaged
        )
    }

    func testErroredProbeNeverReadsAsForeground() {
        // The A3 guard: a socket answering + a probe error must be indeterminate,
        // NEVER foreground — the app must never tell an operator to kill their
        // hand-run daemon because a probe wedged.
        XCTAssertEqual(
            DaemonOwnership.classify(inputs(brewProbe: .indeterminate, socketAnswers: true)),
            .indeterminate
        )
    }

    // MARK: - Persisted manual plist (the dormant-installer blind spot)

    func testPersistedManualPlistIsForeignNotUnmanaged() {
        // A manual install that was `bootout`'d but whose plist still sits in
        // ~/Library/LaunchAgents reads as notRegistered on both probes and a silent
        // socket — but launchd auto-loads that plist at next login, so installing the app
        // agent now would create a same-label conflict. The on-disk plist is an ownership
        // signal: it must surface as foreignManual, never unmanaged (the install verdict).
        XCTAssertEqual(DaemonOwnership.classify(inputs(manualPlistPersisted: true)), .foreignManual)
    }

    func testPersistedManualPlistDefersApproval() {
        // The same dormant plist must also block the approval all-clear — approving our
        // agent would contend with the plist launchd reloads at next login.
        XCTAssertEqual(
            DaemonOwnership.classify(inputs(selfState: .requiresApproval, manualPlistPersisted: true)),
            .indeterminate
        )
    }

    func testSelfIdentityStillWinsOverADormantManualPlist() {
        // If our agent is enabled, WE manage the daemon now; a dormant manual plist is a
        // separate latent cleanup, not a reason to call our own daemon foreign. Install
        // isn't gated when selfManaged anyway, so self-identity still resolves first.
        XCTAssertEqual(
            DaemonOwnership.classify(inputs(selfState: .installed, manualPlistPersisted: true)),
            .selfManaged
        )
    }

    func testSharedLabelDisambiguatedBySelfStatus() {
        // The crux: the same canonical label, ours vs. theirs, split by self-status.
        XCTAssertEqual(
            DaemonOwnership.classify(inputs(selfState: .installed, canonicalProbe: .registered)),
            .selfManaged
        )
        XCTAssertEqual(
            DaemonOwnership.classify(inputs(selfState: .notInstalled, canonicalProbe: .registered)),
            .foreignManual
        )
    }

    // MARK: - Collisions (every competing owner the single verdict hides)

    func testNoCollisionsOnAPristineHost() {
        let c = DaemonOwnership.collisions(inputs())
        XCTAssertEqual(c, DaemonOwnershipCollisions())
        XCTAssertFalse(c.manual)
    }

    func testSelfManagedHidesADormantManualPlist() {
        // #101a: our agent is enabled (verdict selfManaged), but a leftover manual
        // plist persists. The verdict names only us; collisions must surface the
        // dormant manual owner, and as a PLIST (rm), never a loaded job (bootout would
        // evict our own agent on the shared label).
        let c = DaemonOwnership.collisions(inputs(selfState: .installed, manualPlistPersisted: true))
        XCTAssertTrue(c.ownAgent)
        XCTAssertTrue(c.manual)
        XCTAssertTrue(c.manualPlist)
        XCTAssertFalse(c.manualLoaded)
    }

    func testForeignBrewHidesACoPresentLoadedManual() {
        // #101b: brew is registered (verdict foreignBrew) AND a manual installer's
        // canonical label is loaded with self NOT installed — so the manual job is
        // foreign and loaded (bootout needed). The brew-only banner hides it.
        let c = DaemonOwnership.collisions(
            inputs(selfState: .notInstalled, brewProbe: .registered, canonicalProbe: .registered)
        )
        XCTAssertTrue(c.brew)
        XCTAssertTrue(c.manual)
        XCTAssertTrue(c.manualLoaded)
        XCTAssertFalse(c.ownAgent)
    }

    func testForeignBrewHidesOurPendingAgent() {
        // #102: brew registered (verdict foreignBrew) while OUR agent is registered but
        // awaiting approval. ownAgent must be true for a pending agent too, so the UI
        // offers the in-app withdrawal a .requiresApproval agent otherwise lacks.
        let c = DaemonOwnership.collisions(inputs(selfState: .requiresApproval, brewProbe: .registered))
        XCTAssertTrue(c.brew)
        XCTAssertTrue(c.ownAgent)
        XCTAssertFalse(c.manual)
    }

    func testLoadedCanonicalIsOursNotManualWhenSelfInstalled() {
        // When self is .installed the loaded canonical label is OURS, so a registered
        // canonical probe must NOT read as a foreign manual load — only a dormant plist
        // would (and here there is none).
        let c = DaemonOwnership.collisions(inputs(selfState: .installed, canonicalProbe: .registered))
        XCTAssertTrue(c.ownAgent)
        XCTAssertFalse(c.manual)
        XCTAssertFalse(c.manualLoaded)
    }

    // MARK: - Manual-cleanup command (rm vs bootout, keyed on who owns the live label)

    func testCleanupCommandIsRmOnlyWhenWeOwnTheLiveLabel() {
        // selfManaged + dormant plist: we own the loaded canonical label, so the
        // command must be `rm` of the plist ONLY — a bootout of the shared label would
        // evict our own running agent.
        let c = DaemonOwnership.collisions(inputs(selfState: .installed, manualPlistPersisted: true))
        let cmd = AgentController.manualCleanupCommand(c)
        XCTAssertNotNil(cmd)
        XCTAssertFalse(cmd!.contains("bootout"), "must not bootout the label our own agent holds")
        XCTAssertTrue(cmd!.contains("rm -f"))
    }

    func testCleanupCommandBootsOutAndRemovesAForeignLoadedManual() {
        // Foreign manual loaded (self not installed) with a plist on disk: bootout the
        // loaded foreign job AND rm the plist that would reload it at next login.
        let c = DaemonOwnership.collisions(
            inputs(selfState: .notInstalled, canonicalProbe: .registered, manualPlistPersisted: true)
        )
        let cmd = AgentController.manualCleanupCommand(c)
        XCTAssertNotNil(cmd)
        XCTAssertTrue(cmd!.contains("bootout"))
        XCTAssertTrue(cmd!.contains("rm -f"))
    }

    func testCleanupCommandIsNilWithoutAManualOwner() {
        XCTAssertNil(AgentController.manualCleanupCommand(DaemonOwnership.collisions(inputs())))
        XCTAssertNil(AgentController.manualCleanupCommand(
            DaemonOwnership.collisions(inputs(selfState: .installed))
        ))
    }
}
