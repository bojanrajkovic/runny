import XCTest

@testable import Runny

/// The pure mappers behind the CLI-install model: the verify→state mapping, the
/// osascript exit→outcome recovery, the translocation predicate, and the shell/
/// AppleScript escaping that keeps a path with spaces or quotes from breaking out
/// of the one privileged command. The FileManager/osascript execution itself is
/// the thin untested shell (live-machine only); everything that DECIDES is here.
final class CLIInstallModelTests: XCTestCase {
    // MARK: - verify → state

    func testStateForVerify() {
        XCTAssertEqual(CLIInstallModel.stateForVerify(.installed), .installed)
        XCTAssertEqual(CLIInstallModel.stateForVerify(.installedButNotOnPath), .installedButNotOnPath)
        // A read-back that resolves elsewhere or finds nothing despite a "success"
        // return is a loud failure, never a green check — the confirm-from-disk rule.
        if case .failed = CLIInstallModel.stateForVerify(.mismatch) {} else { XCTFail("mismatch must fail loudly") }
        if case .failed = CLIInstallModel.stateForVerify(.missing) {} else { XCTFail("missing must fail loudly") }
    }

    // MARK: - osascript exit → outcome

    func testOutcomeOk() {
        XCTAssertEqual(CLIInstallModel.outcomeForOsascript(exitCode: 0, stderr: ""), .ok)
    }

    func testOutcomeCancelled() {
        // A user-dismissed admin prompt is AppleScript error -128.
        XCTAssertEqual(
            CLIInstallModel.outcomeForOsascript(exitCode: 1, stderr: "execution error: User canceled. (-128)"),
            .cancelled
        )
    }

    func testOutcomeRefusedForeign() {
        // The foreign guard inside `do shell script` exits 3; osascript surfaces it
        // as the trailing parenthesized code.
        XCTAssertEqual(
            CLIInstallModel.outcomeForOsascript(exitCode: 1, stderr: "execution error: ... (3)"),
            .refusedForeign
        )
    }

    func testOutcomeFailed() {
        if case .failed = CLIInstallModel.outcomeForOsascript(exitCode: 1, stderr: "some other error (5)") {} else {
            XCTFail("an unrecognized nonzero exit must be a loud failure")
        }
        // A "(3)" embedded earlier must NOT be read as a foreign-refusal when the
        // real trailing exit code is something else — the precise-parse guard that
        // keeps a genuine failure from being silently downgraded to .conflict.
        if case .failed = CLIInstallModel.outcomeForOsascript(exitCode: 1, stderr: "ln: /Volumes/Disk (3)/x: failed (1)") {} else {
            XCTFail("embedded (3) must not misclassify a real failure as refusedForeign")
        }
        // Empty stderr still yields a non-empty message (no blank failure).
        if case let .failed(m) = CLIInstallModel.outcomeForOsascript(exitCode: 2, stderr: "") {
            XCTAssertFalse(m.isEmpty)
        } else {
            XCTFail("expected failed")
        }
    }

    func testTrailingParenCode() {
        XCTAssertEqual(CLIInstallModel.trailingParenCode("execution error: ... (3)"), 3)
        XCTAssertEqual(CLIInstallModel.trailingParenCode("User canceled. (-128)"), -128)
        // The LAST parenthesized code wins; an earlier "(3)" is ignored.
        XCTAssertEqual(CLIInstallModel.trailingParenCode("path (3)/x failed (1)"), 1)
        XCTAssertNil(CLIInstallModel.trailingParenCode("no trailing code here"))
        XCTAssertNil(CLIInstallModel.trailingParenCode(""))
    }

    func testReachableUnionsProcessAndSystemPath() {
        // The fix case: a Finder-launched app's process PATH omits /usr/local/bin,
        // but the system source (/etc/paths) has it → reachable, no false warning.
        XCTAssertTrue(CLIInstallModel.reachable(
            processPath: "/usr/bin:/bin", systemPath: "/usr/local/bin:/usr/bin:/bin",
            dir: "/usr/local/bin"
        ))
        // In neither source → genuinely not reachable (the /opt/homebrew-only case).
        XCTAssertFalse(CLIInstallModel.reachable(
            processPath: "/usr/bin:/bin", systemPath: "/usr/bin:/bin",
            dir: "/usr/local/bin"
        ))
        // Present in the process PATH alone is enough.
        XCTAssertTrue(CLIInstallModel.reachable(
            processPath: "/usr/local/bin:/usr/bin", systemPath: "",
            dir: "/usr/local/bin"
        ))
    }

    func testPathContainsNormalizesTrailingSlash() {
        XCTAssertTrue(CLIInstallModel.pathContains("/usr/bin:/usr/local/bin:/bin", dir: "/usr/local/bin"))
        // A trailing slash on the PATH entry (or the queried dir) is the same dir.
        XCTAssertTrue(CLIInstallModel.pathContains("/usr/bin:/usr/local/bin/:/bin", dir: "/usr/local/bin"))
        XCTAssertTrue(CLIInstallModel.pathContains("/usr/bin:/usr/local/bin:/bin", dir: "/usr/local/bin/"))
        XCTAssertFalse(CLIInstallModel.pathContains("/usr/bin:/opt/homebrew/bin", dir: "/usr/local/bin"))
        XCTAssertFalse(CLIInstallModel.pathContains("", dir: "/usr/local/bin"))
    }

    // MARK: - translocation

    func testIsTranslocated() {
        XCTAssertTrue(CLIInstallModel.isTranslocated("/private/var/folders/ab/cd/T/AppTranslocation/UUID/d/Runny.app"))
        XCTAssertTrue(CLIInstallModel.isTranslocated("/private/var/folders/xy/z/Runny.app"))
        XCTAssertFalse(CLIInstallModel.isTranslocated("/Applications/Runny.app"))
        XCTAssertFalse(CLIInstallModel.isTranslocated("/Users/me/Applications/Runny.app"))
    }

    // MARK: - shell + AppleScript escaping (security-sensitive)

    func testShellSingleQuotePlain() {
        XCTAssertEqual(CLIInstallModel.shellSingleQuote("/Applications/Runny.app"), "'/Applications/Runny.app'")
    }

    func testShellSingleQuoteWithSpace() {
        XCTAssertEqual(
            CLIInstallModel.shellSingleQuote("/Users/me/My Apps/Runny.app/Contents/MacOS/runnyctl"),
            "'/Users/me/My Apps/Runny.app/Contents/MacOS/runnyctl'"
        )
    }

    func testShellSingleQuoteEscapesQuote() {
        // A single quote in the path must close-escape-reopen, never break out.
        XCTAssertEqual(CLIInstallModel.shellSingleQuote("/a'b"), "'/a'\\''b'")
    }

    func testAppleScriptEscaping() {
        // Backslashes and double-quotes are escaped for the AppleScript string layer.
        XCTAssertEqual(
            CLIInstallModel.appleScript(doShell: "echo \"hi\" \\ there"),
            "do shell script \"echo \\\"hi\\\" \\\\ there\" with administrator privileges"
        )
    }

    func testInstallScriptStructure() {
        let s = CLIInstallModel.installScript(target: "/Applications/Runny.app/Contents/MacOS/runnyctl")
        XCTAssertTrue(s.contains("with administrator privileges"))
        // Non-forcing create (single-quoted target through the AppleScript layer):
        // never `ln -sfn`, whose -f would clobber a foreign file that raced in after
        // the guard.
        XCTAssertTrue(s.contains("ln -s '/Applications/Runny.app/Contents/MacOS/runnyctl'"))
        XCTAssertFalse(s.contains("ln -sfn"))
        // The write-time guard removes ONLY a Runny.app link, then refuses (exit 3)
        // anything still at the path — a foreign file, or one that raced in.
        XCTAssertTrue(s.contains("*/Runny.app/Contents/MacOS/runnyctl) rm -f"))
        XCTAssertTrue(s.contains("if [ -e /usr/local/bin/runnyctl ] || [ -L /usr/local/bin/runnyctl ]; then exit 3"))
        // mkdir -p so a missing /usr/local/bin is created under escalation.
        XCTAssertTrue(s.contains("mkdir -p /usr/local/bin"))
    }

    func testResolvedLinkFollowsDanglingRunnyTarget() throws {
        let fm = FileManager.default
        let dir = fm.temporaryDirectory.appendingPathComponent("runny-resolvedlink-\(UUID().uuidString)")
        try fm.createDirectory(at: dir, withIntermediateDirectories: true)
        defer { try? fm.removeItem(at: dir) }
        let link = dir.appendingPathComponent("runnyctl").path
        // A link into a Runny.app that does NOT exist (the app was moved/deleted):
        // resolvingSymlinksInPath can't reach it, so the resolver must fall back to
        // the raw destination — otherwise a stale Runny link reads as foreign.
        let deadTarget = dir.appendingPathComponent("Runny.app/Contents/MacOS/runnyctl").path
        try fm.createSymbolicLink(atPath: link, withDestinationPath: deadTarget)
        let resolved = CLIInstallModel.resolvedLink(link)
        XCTAssertEqual(resolved, deadTarget)
        XCTAssertTrue(CLIInstall.isRunnyBundleCLI(resolved ?? ""),
                      "a dangling Runny.app link must classify as Runny-owned, not foreign")
    }

    func testRemoveScriptOnlyTouchesThisBundle() {
        let s = CLIInstallModel.removeScript(target: "/Applications/Runny.app/Contents/MacOS/runnyctl")
        XCTAssertTrue(s.contains("with administrator privileges"))
        // Removes only a link pointing at THIS bundle (exact compare, not a Runny.app
        // glob that would match another copy's link); anything else exits 3. (The
        // `"$existing"` quotes are escaped by the AppleScript layer, so assert on the
        // single-quoted target + rm, which pass through verbatim.)
        XCTAssertTrue(s.contains("= '/Applications/Runny.app/Contents/MacOS/runnyctl' ]; then rm -f /usr/local/bin/runnyctl"))
        XCTAssertTrue(s.contains("exit 3"))
        XCTAssertFalse(s.contains("*/Runny.app/Contents/MacOS/runnyctl)"), "must not use the loose Runny.app glob")
    }

    // MARK: - Orphan reconcile + cleanup

    private static let bundle = "/Applications/Runny.app/Contents/MacOS/runnyctl"

    func testRestingClassificationSurfacesADanglingRunnyLinkAsOrphan() {
        // A link into a DIFFERENT Runny.app whose bundle is gone (targetExists=false):
        // the orphan a drag-to-trash leaves. It must surface as .orphaned, not the
        // silent .notInstalled the old code returned.
        let dead = "/Volumes/Old/Runny.app/Contents/MacOS/runnyctl"
        XCTAssertEqual(
            CLIInstallModel.restingClassification(
                existing: .symlink(resolvesTo: dead), bundle: Self.bundle, onPath: true, targetExists: false
            ),
            .orphaned(target: dead)
        )
    }

    func testRestingClassificationKeepsLiveOtherRunnyLinkInstallable() {
        // A link into another Runny.app that STILL exists (targetExists=true) is
        // re-pointable on install — it stays .notInstalled, never an orphan.
        let live = "/Users/me/Applications/Runny.app/Contents/MacOS/runnyctl"
        XCTAssertEqual(
            CLIInstallModel.restingClassification(
                existing: .symlink(resolvesTo: live), bundle: Self.bundle, onPath: true, targetExists: true
            ),
            .notInstalled
        )
    }

    func testRestingClassificationInstalledConflictAbsent() {
        // This bundle → installed/installedButNotOnPath by PATH; a foreign symlink and
        // a regular file → conflict; absent → notInstalled. targetExists is irrelevant
        // once the link resolves to this bundle.
        XCTAssertEqual(
            CLIInstallModel.restingClassification(
                existing: .symlink(resolvesTo: Self.bundle), bundle: Self.bundle, onPath: true, targetExists: true
            ),
            .installed
        )
        XCTAssertEqual(
            CLIInstallModel.restingClassification(
                existing: .symlink(resolvesTo: Self.bundle), bundle: Self.bundle, onPath: false, targetExists: true
            ),
            .installedButNotOnPath
        )
        XCTAssertEqual(
            CLIInstallModel.restingClassification(
                existing: .symlink(resolvesTo: "/opt/homebrew/bin/runnyctl"), bundle: Self.bundle,
                onPath: true, targetExists: true
            ),
            .conflict(owner: "/opt/homebrew/bin/runnyctl")
        )
        XCTAssertEqual(
            CLIInstallModel.restingClassification(
                existing: .regularFile, bundle: Self.bundle, onPath: true, targetExists: false
            ),
            .conflict(owner: CLIInstallModel.linkPath)
        )
        XCTAssertEqual(
            CLIInstallModel.restingClassification(
                existing: .absent, bundle: Self.bundle, onPath: true, targetExists: false
            ),
            .notInstalled
        )
    }

    func testRemoveOrphanScriptRemovesOnlyADeadRunnyLink() {
        let s = CLIInstallModel.removeOrphanScript()
        XCTAssertTrue(s.contains("with administrator privileges"))
        // Removes a Runny.app link ONLY when its target is gone (`[ -e "$existing" ]`
        // re-check at write time): a reappeared target → exit 0, leave the now-live
        // link; a foreign file still present → exit 3. No create. This is deliberately
        // NOT installScript's unconditional remove-any-Runny-link.
        XCTAssertTrue(s.contains("*/Runny.app/Contents/MacOS/runnyctl)"))
        XCTAssertTrue(s.contains("if [ -e \\\"$existing\\\" ]; then exit 0; else rm -f /usr/local/bin/runnyctl; fi"))
        XCTAssertTrue(s.contains("if [ -e /usr/local/bin/runnyctl ] || [ -L /usr/local/bin/runnyctl ]; then exit 3"))
        XCTAssertFalse(s.contains("ln -s"), "orphan cleanup removes, never creates")
    }

    func testRemoveOrphanUnprivilegedRemovesDeadLinkButLeavesLiveAndForeign() throws {
        let fm = FileManager.default
        let dir = fm.temporaryDirectory.appendingPathComponent("runny-orphan-\(UUID().uuidString)")
        try fm.createDirectory(at: dir, withIntermediateDirectories: true)
        defer { try? fm.removeItem(at: dir) }

        // A dangling Runny-owned link (target bundle missing, never created) → removed.
        let dead = dir.appendingPathComponent("dead-runnyctl").path
        try fm.createSymbolicLink(atPath: dead, withDestinationPath: dir.appendingPathComponent("missing/Runny.app/Contents/MacOS/runnyctl").path)
        XCTAssertNil(CLIInstallModel.removeOrphanUnprivileged(dead))
        XCTAssertNil(try? fm.destinationOfSymbolicLink(atPath: dead), "a dead orphan link must be removed")

        // A Runny-owned link whose target REAPPEARED (live again) → left in place, nil
        // returned so the read-back re-derives — never delete a working other-copy link.
        let liveTargetDir = dir.appendingPathComponent("here/Runny.app/Contents/MacOS")
        try fm.createDirectory(at: liveTargetDir, withIntermediateDirectories: true)
        let liveTarget = liveTargetDir.appendingPathComponent("runnyctl").path
        fm.createFile(atPath: liveTarget, contents: Data("bin".utf8))
        let live = dir.appendingPathComponent("live-runnyctl").path
        try fm.createSymbolicLink(atPath: live, withDestinationPath: liveTarget)
        XCTAssertNil(CLIInstallModel.removeOrphanUnprivileged(live))
        XCTAssertNotNil(try? fm.destinationOfSymbolicLink(atPath: live), "a link to a live bundle must be left untouched")

        // A foreign symlink → refused, left in place.
        let foreign = dir.appendingPathComponent("foreign").path
        try fm.createSymbolicLink(atPath: foreign, withDestinationPath: "/opt/homebrew/bin/runnyctl")
        XCTAssertNotNil(CLIInstallModel.removeOrphanUnprivileged(foreign))
        XCTAssertNotNil(try? fm.destinationOfSymbolicLink(atPath: foreign), "a foreign link must be left untouched")

        // Absent → no-op success.
        XCTAssertNil(CLIInstallModel.removeOrphanUnprivileged(dir.appendingPathComponent("nothing").path))
    }
}
