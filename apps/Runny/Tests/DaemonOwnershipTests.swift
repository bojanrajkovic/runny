import XCTest

@testable import Runny

/// The pure ownership verdict: `classify` over three facts — the home's
/// canonicity, the app's SMAppService self-status, and whether a system
/// LaunchDaemon is registered. The ordering is load-bearing (home defers first,
/// a registered system daemon wins, a wedged system probe fails closed, then the
/// per-user self-status), so every branch is pinned without launchd or a live
/// SMAppService.
final class DaemonOwnershipTests: XCTestCase {
    /// A clear, install-allowed baseline; each test overrides only what it exercises.
    private func inputs(
        homeIsCanonical: Bool = true,
        selfState: LaunchAgentStatus.State = .notInstalled,
        systemProbe: LaunchdProbeResult = .notRegistered
    ) -> DaemonOwnershipInputs {
        DaemonOwnershipInputs(
            homeIsCanonical: homeIsCanonical, selfState: selfState, systemProbe: systemProbe
        )
    }

    func testUnmanagedWhenNothingOwnsIt() {
        // Home canonical, no system daemon, our agent not installed → install allowed.
        XCTAssertEqual(DaemonOwnership.classify(inputs()), .unmanaged)
    }

    func testNotFoundSelfStatusIsUnmanaged() {
        // A dev build with no bundled agent reads `.notFound`, not `.notInstalled`, but
        // is still "nothing ours here" — install-allowed, not a deferral.
        XCTAssertEqual(DaemonOwnership.classify(inputs(selfState: .notFound)), .unmanaged)
    }

    func testSelfManagedWhenOurAgentEnabled() {
        // `.installed` == SMAppService `.enabled` == ours, with no system daemon present.
        XCTAssertEqual(DaemonOwnership.classify(inputs(selfState: .installed)), .selfManaged)
    }

    func testAwaitingApprovalWhenOurAgentUnapproved() {
        XCTAssertEqual(DaemonOwnership.classify(inputs(selfState: .requiresApproval)), .awaitingApproval)
    }

    func testSystemManagedWhenSystemLabelRegistered() {
        // A runnyd registered in the system/ domain — the installed non-root daemon —
        // the app observes over the shared socket and never installs a per-user agent over.
        XCTAssertEqual(DaemonOwnership.classify(inputs(systemProbe: .registered)), .systemManaged)
    }

    func testSystemManagedWinsOverOurOwnEnabledAgent() {
        // Top precedence: a system daemon owns the shared socket the app dials first, so it
        // surfaces even when our own agent is also enabled — a real two-manager conflict the
        // install gate must keep the per-user agent out of, not a self-managed host.
        XCTAssertEqual(
            DaemonOwnership.classify(inputs(selfState: .installed, systemProbe: .registered)),
            .systemManaged
        )
    }

    func testWedgedSystemProbeFailsClosed() {
        // A wedged/ambiguous system/ probe must fail CLOSED — a system daemon MIGHT be
        // here — so it defers, never falls through to unmanaged (which would install a
        // competing per-user agent over it).
        XCTAssertEqual(DaemonOwnership.classify(inputs(systemProbe: .indeterminate)), .indeterminate)
    }

    func testWedgedSystemProbeFailsClosedEvenWithSelfInstalled() {
        // The fail-closed system probe is checked BEFORE the per-user self-status: an
        // enabled own agent does NOT override a wedged system probe, because if a system
        // daemon is in fact present it outranks our per-user agent (and managing/reinstalling
        // the agent over it is the orphaned-per-user-behind-system stomp). So an inconclusive
        // system probe defers even when self reads `.installed`.
        XCTAssertEqual(
            DaemonOwnership.classify(inputs(selfState: .installed, systemProbe: .indeterminate)),
            .indeterminate
        )
    }

    func testWedgedSystemProbeDefersApproval() {
        // The same wedge must block the approval all-clear: approving our agent over a
        // possibly-present system daemon would create a competing manager.
        XCTAssertEqual(
            DaemonOwnership.classify(inputs(selfState: .requiresApproval, systemProbe: .indeterminate)),
            .indeterminate
        )
    }

    func testUnknownSelfStatusFailsClosed() {
        // An unrecognized future SMAppService status maps to `.registrationFailed` — a
        // determination FAILURE, not a confirmed not-installed. Defer (never install)
        // rather than treat unknown registration state as install permission.
        XCTAssertEqual(
            DaemonOwnership.classify(inputs(selfState: .registrationFailed(reason: "x"))),
            .indeterminate
        )
    }

    func testNonCanonicalHomeDefersFirstAheadOfEverything() {
        // Defense-in-depth: even with a registered system daemon, a non-canonical home
        // means the socket and artifact axes would describe different homes — defer ahead
        // of every positive branch.
        XCTAssertEqual(
            DaemonOwnership.classify(inputs(homeIsCanonical: false, systemProbe: .registered)),
            .indeterminate
        )
    }
}
