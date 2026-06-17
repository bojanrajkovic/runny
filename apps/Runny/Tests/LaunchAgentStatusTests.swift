import ServiceManagement
import XCTest

@testable import Runny

/// The lifecycle module's pure verdicts: the SMAppService.Status → State map, the
/// install-location eligibility (with the translocation/first-launch distinction),
/// and the canonical-agent reconcile compare. Pure, so every branch is pinned
/// without launchd or a live SMAppService.
final class LaunchAgentStatusTests: XCTestCase {
    func testStatusMapsToNamedStateNeverSilentNotInstalled() {
        XCTAssertEqual(LaunchAgentStatus.state(from: .notRegistered), .notInstalled)
        XCTAssertEqual(LaunchAgentStatus.state(from: .enabled), .installed)
        XCTAssertEqual(LaunchAgentStatus.state(from: .requiresApproval), .requiresApproval)
        XCTAssertEqual(LaunchAgentStatus.state(from: .notFound), .notFound)
    }

    func testEligibleOnlyInApplicationsAndNotTranslocated() {
        XCTAssertEqual(
            LaunchAgentStatus.eligibility(bundlePath: "/Applications/Runny.app", translocated: false),
            .eligible
        )
    }

    func testTranslocatedIsRecoverableNotRefused() {
        // The first-launch-quarantine case: even an /Applications path that is
        // currently translocated must be the recoverable retry verdict, never a
        // permanent refusal.
        XCTAssertEqual(
            LaunchAgentStatus.eligibility(bundlePath: "/Applications/Runny.app", translocated: true),
            .translocated
        )
        XCTAssertEqual(
            LaunchAgentStatus.eligibility(
                bundlePath: "/private/var/folders/xy/AppTranslocation/ABC/d/Runny.app",
                translocated: true
            ),
            .translocated
        )
    }

    func testNotInApplicationsRefusedWithPath() {
        XCTAssertEqual(
            LaunchAgentStatus.eligibility(
                bundlePath: "/Users/someone/Downloads/Runny.app",
                translocated: false
            ),
            .notInApplications(path: "/Users/someone/Downloads/Runny.app")
        )
    }

    func testCanToggleAllowsUninstallFromAnyLocationButGatesInstall() {
        // Installed (or approval-pending) → always toggle-able so a stale agent can
        // be uninstalled, even from a translocated or moved bundle.
        XCTAssertTrue(LaunchAgentStatus.canToggle(state: .installed, eligibility: .translocated))
        XCTAssertTrue(LaunchAgentStatus.canToggle(state: .requiresApproval, eligibility: .notInApplications(path: "/x")))
        // Not installed → only from an eligible location (install gate).
        XCTAssertTrue(LaunchAgentStatus.canToggle(state: .notInstalled, eligibility: .eligible))
        XCTAssertFalse(LaunchAgentStatus.canToggle(state: .notInstalled, eligibility: .translocated))
        // No bundled plist → nothing to register or remove.
        XCTAssertFalse(LaunchAgentStatus.canToggle(state: .notFound, eligibility: .eligible))
    }

    func testReconcileComparesCanonicalProgramNotRunningBundle() {
        XCTAssertTrue(
            LaunchAgentStatus.isCanonicalAgentProgram("/Applications/Runny.app/Contents/MacOS/runnyd")
        )
        // A translocated mount's program path is NOT canonical — but the reconcile
        // must compare the OBSERVED agent program against the canonical location,
        // so an agent pointing into /Applications stays good even when this very
        // process is running translocated.
        XCTAssertFalse(
            LaunchAgentStatus.isCanonicalAgentProgram(
                "/private/var/folders/xy/AppTranslocation/ABC/d/Runny.app/Contents/MacOS/runnyd"
            )
        )
        XCTAssertFalse(LaunchAgentStatus.isCanonicalAgentProgram("/usr/local/bin/runnyd"))
    }
}
