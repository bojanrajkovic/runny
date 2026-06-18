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
        socketAnswers: Bool = false
    ) -> DaemonOwnershipInputs {
        DaemonOwnershipInputs(
            homeIsCanonical: homeIsCanonical, selfState: selfState,
            brewProbe: brewProbe, canonicalProbe: canonicalProbe, socketAnswers: socketAnswers
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
        // With nothing else present it stays the approval CTA.
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
        XCTAssertEqual(
            DaemonOwnership.classify(inputs(selfState: .installed, brewProbe: .indeterminate)),
            .selfManaged
        )
        XCTAssertEqual(
            DaemonOwnership.classify(inputs(selfState: .requiresApproval, canonicalProbe: .indeterminate)),
            .awaitingApproval
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
}
