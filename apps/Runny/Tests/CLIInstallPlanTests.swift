import XCTest

@testable import Runny

/// The CLI-install decision core: every `plan` verdict and `verify` result,
/// pinned without touching `/usr/local/bin` or raising an admin prompt. The
/// two safety invariants — never clobber a foreign file, fail closed from a
/// translocated bundle — get their own named cases, since a regression in
/// either is a silent install that dangles or overwrites someone else's binary.
final class CLIInstallPlanTests: XCTestCase {
    private static let bundle = "/Applications/Runny.app/Contents/MacOS/runnyctl"

    private func installVerdict(
        existing: CLIInstall.Existing = .absent,
        dirWritable: Bool = true,
        translocated: Bool = false
    ) -> CLIInstall.Verdict {
        CLIInstall.plan(
            intent: .install, bundleCLIPath: Self.bundle,
            existing: existing, dirWritable: dirWritable, translocated: translocated
        )
    }

    private func uninstallVerdict(existing: CLIInstall.Existing) -> CLIInstall.Verdict {
        CLIInstall.plan(
            intent: .uninstall, bundleCLIPath: Self.bundle,
            existing: existing, dirWritable: true, translocated: false
        )
    }

    // MARK: - Install

    func testAbsentWritableCreatesUnprivileged() {
        XCTAssertEqual(installVerdict(existing: .absent, dirWritable: true), .createUnprivileged)
    }

    func testAbsentNotWritableEscalates() {
        // /usr/local/bin missing or root-owned (a fresh Apple-Silicon host): the
        // privileged step's mkdir -p + write needs admin.
        XCTAssertEqual(installVerdict(existing: .absent, dirWritable: false), .escalate)
    }

    func testTranslocatedRefusesEvenWhenOtherwiseInstallable() {
        // Fail closed: a link into /private/var/folders/.../Runny.app evaporates
        // on next launch. Translocation wins over a perfectly installable state.
        XCTAssertEqual(installVerdict(existing: .absent, dirWritable: true, translocated: true), .refuseTranslocated)
    }

    func testTranslocatedBeatsForeign() {
        // Pins the precedence: our bundle being unusable is the more fundamental
        // blocker than a foreign file (fixing the file wouldn't make a translocated
        // bundle linkable).
        XCTAssertEqual(
            installVerdict(existing: .regularFile, translocated: true),
            .refuseTranslocated
        )
    }

    func testThisBundleAlreadyInstalled() {
        XCTAssertEqual(installVerdict(existing: .symlink(resolvesTo: Self.bundle)), .alreadyInstalled)
    }

    func testOtherRunnyBundleRepoints() {
        // A link into a DIFFERENT Runny.app (the app moved or was reinstalled): we
        // adopt it onto this bundle's path — a create, escalation by dir writability.
        let other = "/Users/me/Applications/Runny.app/Contents/MacOS/runnyctl"
        XCTAssertEqual(installVerdict(existing: .symlink(resolvesTo: other), dirWritable: true), .createUnprivileged)
        XCTAssertEqual(installVerdict(existing: .symlink(resolvesTo: other), dirWritable: false), .escalate)
    }

    func testForeignSymlinkRefuses() {
        // A brew-managed runnyctl (the P5 reconciliation case) — never overwrite it.
        XCTAssertEqual(
            installVerdict(existing: .symlink(resolvesTo: "/opt/homebrew/bin/runnyctl")),
            .refuseForeign
        )
    }

    func testRegularFileRefuses() {
        XCTAssertEqual(installVerdict(existing: .regularFile), .refuseForeign)
    }

    // MARK: - Uninstall

    func testUninstallThisBundleRemovesOurs() {
        XCTAssertEqual(uninstallVerdict(existing: .symlink(resolvesTo: Self.bundle)), .removeOurs)
    }

    func testUninstallAbsentIsNotInstalled() {
        XCTAssertEqual(uninstallVerdict(existing: .absent), .notInstalled)
    }

    func testUninstallOtherRunnyBundleRefuses() {
        // Another Runny.app copy owns this link; this bundle doesn't remove it.
        let other = "/Volumes/Backup/Runny.app/Contents/MacOS/runnyctl"
        XCTAssertEqual(uninstallVerdict(existing: .symlink(resolvesTo: other)), .refuseForeign)
    }

    func testUninstallForeignRefuses() {
        XCTAssertEqual(uninstallVerdict(existing: .regularFile), .refuseForeign)
    }

    // MARK: - Verify (read-back)

    func testVerifyInstalled() {
        XCTAssertEqual(
            CLIInstall.verify(bundleCLIPath: Self.bundle, resolvedLink: Self.bundle, onPath: true),
            .installed
        )
    }

    func testVerifyInstalledButNotOnPath() {
        // The link resolves to this bundle but /usr/local/bin isn't on PATH —
        // "installed yet command not found", common on /opt/homebrew-only hosts.
        XCTAssertEqual(
            CLIInstall.verify(bundleCLIPath: Self.bundle, resolvedLink: Self.bundle, onPath: false),
            .installedButNotOnPath
        )
    }

    func testVerifyMismatch() {
        XCTAssertEqual(
            CLIInstall.verify(bundleCLIPath: Self.bundle, resolvedLink: "/opt/homebrew/bin/runnyctl", onPath: true),
            .mismatch
        )
    }

    func testVerifyMissing() {
        XCTAssertEqual(
            CLIInstall.verify(bundleCLIPath: Self.bundle, resolvedLink: nil, onPath: true),
            .missing
        )
    }

    // MARK: - Runny-bundle recognition

    func testRunnyBundleCLIRecognition() {
        XCTAssertTrue(CLIInstall.isRunnyBundleCLI("/Applications/Runny.app/Contents/MacOS/runnyctl"))
        // Translocated path is still a Runny.app link — recognized as managed.
        XCTAssertTrue(CLIInstall.isRunnyBundleCLI("/private/var/folders/x/y/Runny.app/Contents/MacOS/runnyctl"))
        XCTAssertFalse(CLIInstall.isRunnyBundleCLI("/usr/local/bin/runnyctl"))
        XCTAssertFalse(CLIInstall.isRunnyBundleCLI("/opt/homebrew/bin/runnyctl"))
        // A bundle whose name merely ENDS in "Runny.app" must not masquerade: the
        // matched suffix begins at a path separator, so "FooRunny.app" fails.
        XCTAssertFalse(CLIInstall.isRunnyBundleCLI("/Apps/FooRunny.app/Contents/MacOS/runnyctl"))
    }
}
