import XCTest

@testable import Runny

/// The pure surface of the system-daemon installer: the brokered install/uninstall
/// shell one-liners (security-sensitive — bundle paths and the operator are
/// interpolated into a command that runs as root), the osascript-result → outcome
/// mapping, and the launchd-probe → state confirmation. The osascript execution and
/// the live launchd probe are the thin untested shell; everything that DECIDES is
/// here.
final class SystemDaemonInstallerTests: XCTestCase {
    // MARK: - install script

    func testInstallScriptStructure() {
        let s = SystemDaemonInstaller.installScript(
            bundleRunnyd: "/Applications/Runny.app/Contents/MacOS/runnyd",
            bundleRunnyctl: "/Applications/Runny.app/Contents/MacOS/runnyctl",
            operator: "alice"
        )
        XCTAssertTrue(s.contains("with administrator privileges"))
        XCTAssertTrue(s.contains("mkdir -p /usr/local/libexec/runny"))
        // Stage to a stable location OUTSIDE the bundle (survives app deletion), not
        // run from the bundle.
        XCTAssertTrue(s.contains("cp '/Applications/Runny.app/Contents/MacOS/runnyd' /usr/local/libexec/runny/runnyd"))
        XCTAssertTrue(s.contains("cp '/Applications/Runny.app/Contents/MacOS/runnyctl' /usr/local/libexec/runny/runnyctl"))
        // rm -f before cp so a re-stage over a RUNNING daemon's binary can't hit
        // ETXTBSY (the old inode lives until the daemon exits; install-daemon's
        // bootstrap restarts onto the new one).
        XCTAssertTrue(s.contains("rm -f /usr/local/libexec/runny/runnyd /usr/local/libexec/runny/runnyctl"))
        // Run the STAGED runnyctl, so install-daemon resolves the STAGED runnyd as its
        // sibling and the plist pins the stable path (not the bundle).
        XCTAssertTrue(s.contains("/usr/local/libexec/runny/runnyctl install-daemon --operator 'alice'"))
    }

    func testInstallScriptQuotesOperatorAndPaths() {
        // A space in a bundle path and a quote in the operator must be single-quoted —
        // never break out of the root command.
        let s = SystemDaemonInstaller.installScript(
            bundleRunnyd: "/Users/me/My Apps/Runny.app/Contents/MacOS/runnyd",
            bundleRunnyctl: "/Users/me/My Apps/Runny.app/Contents/MacOS/runnyctl",
            operator: "a'b"
        )
        XCTAssertTrue(s.contains("cp '/Users/me/My Apps/Runny.app/Contents/MacOS/runnyd' /usr/local/libexec/runny/runnyd"))
        // The single quote closes-escapes-reopens the standard '\'' way; the lone
        // backslash is then DOUBLED by the AppleScript string layer, so the final
        // command carries `'a'\\''b'`.
        XCTAssertTrue(s.contains("--operator 'a'\\\\''b'"))
    }

    // MARK: - uninstall script

    func testUninstallScriptUsesBundleRunnyctl() {
        // Uninstall runs the BUNDLE's runnyctl (always present with the app), not the
        // staged copy — so it works even if libexec was cleaned. uninstall-daemon needs
        // no runnyd sibling.
        let s = SystemDaemonInstaller.uninstallScript(bundleRunnyctl: "/Applications/Runny.app/Contents/MacOS/runnyctl")
        XCTAssertTrue(s.contains("with administrator privileges"))
        XCTAssertTrue(s.contains("'/Applications/Runny.app/Contents/MacOS/runnyctl' uninstall-daemon"))
        XCTAssertFalse(s.contains("/usr/local/libexec"), "uninstall must not depend on the staged copy")
    }

    // MARK: - osascript result → outcome

    func testOutcomeOk() {
        XCTAssertEqual(SystemDaemonInstaller.outcome(for: .completed(exitCode: 0, stderr: "")), .ok)
    }

    func testOutcomeCancelled() {
        XCTAssertEqual(
            SystemDaemonInstaller.outcome(for: .completed(exitCode: 1, stderr: "execution error: User canceled. (-128)")),
            .cancelled
        )
    }

    func testOutcomeFailedCarriesStderr() {
        // Unlike the CLI installer there is NO refusedForeign branch — any non-cancel
        // nonzero is a failure that surfaces the daemon installer's own stderr.
        if case let .failed(m) = SystemDaemonInstaller.outcome(for: .completed(exitCode: 1, stderr: "operator account \"x\" does not resolve (1)")) {
            XCTAssertTrue(m.contains("does not resolve"))
        } else {
            XCTFail("a nonzero non-cancel exit must be a loud failure carrying stderr")
        }
        // A launch failure surfaces too.
        if case .failed = SystemDaemonInstaller.outcome(for: .launchFailed("could not launch osascript")) {} else {
            XCTFail("a launch failure must be a loud failure")
        }
        // Empty stderr still yields a non-empty message.
        if case let .failed(m) = SystemDaemonInstaller.outcome(for: .completed(exitCode: 2, stderr: "")) {
            XCTAssertFalse(m.isEmpty)
        } else {
            XCTFail("expected failed")
        }
    }

    // MARK: - launchd probe → confirmation state

    func testStateForInstallProbe() {
        // The install is confirmed by the launchd system job being REGISTERED — not by
        // the daemon answering the socket, which it won't until config is in place.
        XCTAssertEqual(SystemDaemonInstaller.stateForInstallProbe(.registered), .installed)
        // Registered-nowhere despite a success exit is a loud failure (requested≠done).
        if case .failed = SystemDaemonInstaller.stateForInstallProbe(.notRegistered) {} else {
            XCTFail("a missing registration after success must fail loudly")
        }
        if case .failed = SystemDaemonInstaller.stateForInstallProbe(.indeterminate) {} else {
            XCTFail("an unconfirmable registration must fail loudly, never a false installed")
        }
    }

    func testStateForUninstallProbe() {
        XCTAssertEqual(SystemDaemonInstaller.stateForUninstallProbe(.notRegistered), .notInstalled)
        // Still registered after a success exit means the uninstall did not take.
        if case .failed = SystemDaemonInstaller.stateForUninstallProbe(.registered) {} else {
            XCTFail("a still-registered daemon after uninstall must fail loudly")
        }
        if case .failed = SystemDaemonInstaller.stateForUninstallProbe(.indeterminate) {} else {
            XCTFail("an unconfirmable removal must fail loudly")
        }
    }
}
