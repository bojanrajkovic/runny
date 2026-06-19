import AppKit
import Foundation
import Observation

/// The imperative shell over `CLIInstall`'s pure verdicts: it resolves the real
/// filesystem state, applies the verdict (an unprivileged `createSymbolicLink`
/// or one `with administrator privileges` shell line), confirms from disk, and
/// drives the `@Observable` state every surface renders. The decision logic is
/// all in `CLIInstall` and the pure mappers below; this file owns only the
/// `FileManager`/`osascript` execution that can't run under test.
@MainActor
@Observable
final class CLIInstallModel {
    enum State: Equatable {
        /// No link, or a stale link into another Runny.app the install would adopt.
        case notInstalled
        /// A privileged or unprivileged write is in flight; the UI shows a cancel.
        case installing
        case installed
        /// Linked, but `/usr/local/bin` isn't on PATH — installed yet not reachable.
        case installedButNotOnPath
        /// A foreign file owns the path; never overwritten. Carries its owner.
        case conflict(owner: String)
        /// A Runny-owned symlink whose target bundle is GONE — the orphan a
        /// drag-to-trash leaves behind, surfaced honestly (not silently as
        /// `notInstalled`) so it can be removed. Carries the dangling target. Install
        /// still re-points it onto the running bundle; Remove clears it.
        case orphaned(target: String)
        /// The app is translocated / at a transient path; refused (fail closed).
        case translocated
        case failed(String)
        case cancelled
    }

    private(set) var state: State = .notInstalled

    /// Monotonic op identity. A cancel bumps it; a late async result whose
    /// captured generation is stale is ignored, so a cancelled op can never
    /// overwrite the loud `.cancelled` state with a result the operator dismissed.
    private var generation = 0
    private var running: Process?

    static let linkPath = "/usr/local/bin/runnyctl"
    private static let binDir = "/usr/local/bin"

    /// This bundle's nested runnyctl, canonicalized — the symlink target. nil for
    /// an unbundled dev build (no `Contents/MacOS/runnyctl`), where install is
    /// simply unavailable rather than wrong.
    static var bundleCLIPath: String? {
        let url = Bundle.main.bundleURL.appendingPathComponent("Contents/MacOS/runnyctl")
        guard FileManager.default.fileExists(atPath: url.path) else { return nil }
        return url.resolvingSymlinksInPath().path
    }

    /// Recompute the resting state from disk — on appear, and after every op. A
    /// no-op while an op is in flight: onAppear (Settings + popover) calls this,
    /// and overwriting .installing mid-prompt would hide Cancel and let a second
    /// tap stack a duplicate admin prompt.
    func refresh() {
        guard state != .installing else { return }
        state = restingState()
    }

    /// The state the disk implies right now. Shared by `refresh()` and the
    /// post-remove read-back, so both encode the install/conflict/onPath rule once.
    /// Resolves the impure inputs (the link's state, whether a symlink's target still
    /// exists, whether the dir is on PATH) and hands them to the pure classifier.
    private func restingState() -> State {
        guard let bundle = Self.bundleCLIPath else {
            return .failed("this build carries no bundled runnyctl")
        }
        let existing = Self.readExisting(Self.linkPath)
        // Whether a symlink's target is still on disk — the signal that tells a live
        // (re-pointable) Runny link from a dangling orphan. `fileExists` follows the
        // link, so it is false exactly when the target bundle is gone.
        let targetExists: Bool = if case let .symlink(resolved) = existing {
            FileManager.default.fileExists(atPath: resolved)
        } else {
            false
        }
        return Self.restingClassification(
            existing: existing, bundle: bundle, onPath: Self.dirOnPath(Self.binDir), targetExists: targetExists
        )
    }

    /// Confirm a removal from disk — the requested≠done invariant for uninstall,
    /// mirroring `confirm()` for install: a privileged remove that returned 0 but
    /// left the link (a race re-created it, a foreign file the guard skipped) must
    /// re-derive the true state, never claim .notInstalled off the exit code.
    private func confirmRemoved(gen: Int) { apply(restingState(), gen: gen) }

    func install() {
        guard state != .installing else { return }
        generation += 1
        // Enter .installing synchronously, before the Task suspends — otherwise a
        // second tap reads the resting state, passes this guard too, and stacks a
        // second admin prompt.
        state = .installing
        let gen = generation
        Task { await runInstall(gen: gen) }
    }

    func uninstall() {
        guard state != .installing else { return }
        generation += 1
        state = .installing
        let gen = generation
        Task { await runUninstall(gen: gen) }
    }

    /// Remove the dangling orphan link a removed Runny.app left behind. Unlike
    /// `uninstall` (which removes only a link into THIS bundle), this removes any
    /// Runny-owned link — the orphan points into a missing OTHER bundle — while still
    /// refusing a foreign file. Surface-initiated only from the `.orphaned` state.
    func removeOrphan() {
        guard state != .installing else { return }
        generation += 1
        state = .installing
        let gen = generation
        Task { await runRemoveOrphan(gen: gen) }
    }

    /// Cancel an in-flight op: terminate the privileged process if one is up and
    /// flip to the loud terminal `.cancelled` — never a silent spinner. The
    /// generation bump makes any late osascript result a no-op.
    func cancel() {
        guard state == .installing else { return }
        generation += 1
        running?.terminate()
        running = nil
        state = .cancelled
    }

    // MARK: - Install / uninstall (imperative shell)

    private func runInstall(gen: Int) async {
        guard let bundle = Self.bundleCLIPath else {
            apply(.failed("this build carries no bundled runnyctl"), gen: gen)
            return
        }
        let verdict = CLIInstall.plan(
            intent: .install,
            bundleCLIPath: bundle,
            existing: Self.readExisting(Self.linkPath),
            dirWritable: Self.dirWritable(Self.binDir),
            translocated: Self.isTranslocated(Bundle.main.bundleURL.path)
        )
        switch verdict {
        case .refuseTranslocated:
            apply(.translocated, gen: gen)
        case .alreadyInstalled:
            confirm(bundle: bundle, gen: gen)
        case .refuseForeign:
            apply(.conflict(owner: Self.resolvedLink(Self.linkPath) ?? Self.linkPath), gen: gen)
        case .createUnprivileged:
            guard gen == generation else { return } // cancelled before the write
            let err = Self.createUnprivileged(linkPath: Self.linkPath, target: bundle)
            if let err { apply(.failed(err), gen: gen) } else { confirm(bundle: bundle, gen: gen) }
        case .escalate:
            guard gen == generation else { return }
            let outcome = await runPrivileged(target: bundle, gen: gen)
            applyPrivileged(outcome, bundle: bundle, gen: gen)
        case .removeOurs, .notInstalled:
            // Not reachable for an install intent; refresh to the truth.
            confirm(bundle: bundle, gen: gen)
        }
    }

    private func runUninstall(gen: Int) async {
        guard let bundle = Self.bundleCLIPath else {
            apply(.failed("this build carries no bundled runnyctl"), gen: gen)
            return
        }
        let verdict = CLIInstall.plan(
            intent: .uninstall,
            bundleCLIPath: bundle,
            existing: Self.readExisting(Self.linkPath),
            dirWritable: Self.dirWritable(Self.binDir),
            translocated: false // removing a link is safe regardless of our bundle's path
        )
        switch verdict {
        case .removeOurs:
            guard gen == generation else { return } // cancelled before the write
            if Self.dirWritable(Self.binDir) {
                let err = Self.removeUnprivileged(Self.linkPath, bundleCLIPath: bundle)
                if let err { apply(.failed(err), gen: gen) } else { confirmRemoved(gen: gen) }
            } else {
                let outcome = await runPrivilegedRemove(target: bundle, gen: gen)
                switch outcome {
                case .ok: confirmRemoved(gen: gen)
                case .cancelled: apply(.cancelled, gen: gen)
                case .refusedForeign: apply(.conflict(owner: Self.resolvedLink(Self.linkPath) ?? Self.linkPath), gen: gen)
                case let .failed(m): apply(.failed(m), gen: gen)
                }
            }
        case .notInstalled:
            apply(.notInstalled, gen: gen)
        case .refuseForeign:
            apply(.conflict(owner: Self.resolvedLink(Self.linkPath) ?? Self.linkPath), gen: gen)
        case .alreadyInstalled, .createUnprivileged, .escalate, .refuseTranslocated:
            confirm(bundle: bundle, gen: gen)
        }
    }

    private func runRemoveOrphan(gen: Int) async {
        // No CLIInstall.plan call: the orphan is, by definition, a Runny-owned link we
        // may always clear, so the action is unconditional removal — the script and the
        // unprivileged helper enforce the safety (refuse a foreign file that raced in).
        if Self.dirWritable(Self.binDir) {
            guard gen == generation else { return } // cancelled before the write
            let err = Self.removeOrphanUnprivileged(Self.linkPath)
            if let err { apply(.failed(err), gen: gen) } else { confirmRemoved(gen: gen) }
        } else {
            guard gen == generation else { return }
            let outcome = await runOsascript(CLIInstallModel.removeOrphanScript(), gen: gen)
            switch outcome {
            case .ok: confirmRemoved(gen: gen)
            case .cancelled: apply(.cancelled, gen: gen)
            case .refusedForeign: apply(.conflict(owner: Self.resolvedLink(Self.linkPath) ?? Self.linkPath), gen: gen)
            case let .failed(m): apply(.failed(m), gen: gen)
            }
        }
    }

    /// Read back the link and set the final state from the pure verify result —
    /// the "requested ≠ done" invariant: the UI flips to installed only on what
    /// disk actually shows, never on a privileged step's exit code.
    private func confirm(bundle: String, gen: Int) {
        let result = CLIInstall.verify(
            bundleCLIPath: bundle,
            resolvedLink: Self.resolvedLink(Self.linkPath),
            onPath: Self.dirOnPath(Self.binDir)
        )
        apply(Self.stateForVerify(result), gen: gen)
    }

    private func applyPrivileged(_ outcome: PrivilegedOutcome, bundle: String, gen: Int) {
        switch outcome {
        case .ok: confirm(bundle: bundle, gen: gen)
        case .cancelled: apply(.cancelled, gen: gen)
        case .refusedForeign: apply(.conflict(owner: Self.resolvedLink(Self.linkPath) ?? Self.linkPath), gen: gen)
        case let .failed(m): apply(.failed(m), gen: gen)
        }
    }

    /// Set state only if this op is still current — a cancelled/superseded op's
    /// late result is dropped.
    private func apply(_ new: State, gen: Int) {
        guard gen == generation else { return }
        state = new
    }

    // MARK: - Privileged escalation (osascript, off the main actor)

    private func runPrivileged(target: String, gen: Int) async -> PrivilegedOutcome {
        await runOsascript(CLIInstallModel.installScript(target: target), gen: gen)
    }

    private func runPrivilegedRemove(target: String, gen: Int) async -> PrivilegedOutcome {
        await runOsascript(CLIInstallModel.removeScript(target: target), gen: gen)
    }

    /// Raise the admin prompt and wait. The app is `LSUIElement`/accessory, so the
    /// prompt is brought forward by activating; the process handle is held so
    /// `cancel()` can terminate it, and a system-dismissed prompt comes back as
    /// `.cancelled`.
    private func runOsascript(_ script: String, gen: Int) async -> PrivilegedOutcome {
        // Bring the app forward so the system admin prompt has focus. Do NOT flip
        // and restore the activation policy here: the install surface is Settings
        // (already .regular), and blindly restoring a saved policy would strand the
        // app .regular if its window closed during the prompt (the LSUIElement bug
        // ActivationCoordinator's remaining-window check exists to avoid).
        NSApp.activate(ignoringOtherApps: true)
        // A Cancel that landed before we got here already bumped the generation —
        // don't raise the prompt at all.
        guard gen == generation else { return .cancelled }

        let proc = Process()
        proc.executableURL = URL(fileURLWithPath: "/usr/bin/osascript")
        proc.arguments = ["-e", script]
        let errPipe = Pipe()
        proc.standardError = errPipe
        proc.standardOutput = Pipe()
        // Publish `running` and launch synchronously ON the main actor: from here to
        // proc.run() there is no suspension, so a Cancel can only land once the
        // process is live (where running?.terminate() reaches it) — never on an
        // unlaunched process that would then orphan a prompt after .cancelled.
        running = proc
        do {
            try proc.run()
        } catch {
            if running === proc { running = nil }
            return .failed("could not launch osascript: \(error.localizedDescription)")
        }
        // Only the blocking wait goes to a background queue.
        let (status, stderr): (Int32, String) = await withCheckedContinuation { cont in
            DispatchQueue.global().async {
                proc.waitUntilExit()
                let data = errPipe.fileHandleForReading.readDataToEndOfFile()
                cont.resume(returning: (proc.terminationStatus, String(data: data, encoding: .utf8) ?? ""))
            }
        }
        if running === proc { running = nil }
        // A cancel between launch and return already set .cancelled and bumped the
        // generation; report cancelled regardless of how the terminated osascript exited.
        if gen != generation { return .cancelled }
        return Self.outcomeForOsascript(exitCode: status, stderr: stderr)
    }
}

// MARK: - Pure mappers (nonisolated static, unit-tested)

extension CLIInstallModel {
    /// The result of the privileged step, recovered from osascript's exit and
    /// stderr (osascript has no structured channel for `do shell script` failures).
    enum PrivilegedOutcome: Equatable {
        case ok
        case refusedForeign
        case cancelled
        case failed(String)
    }

    /// The resting state the disk implies, as a pure function of the resolved inputs
    /// so the install/conflict/orphan classification is unit-tested without touching
    /// `/usr/local/bin`. `targetExists` distinguishes a live Runny link (install
    /// re-points it) from a dangling orphan into a now-missing bundle (surfaced for
    /// cleanup) — the #89 case a bare `notInstalled` used to hide.
    nonisolated static func restingClassification(
        existing: CLIInstall.Existing, bundle: String, onPath: Bool, targetExists: Bool
    ) -> State {
        switch existing {
        case .absent:
            return .notInstalled
        case .regularFile:
            return .conflict(owner: linkPath)
        case let .symlink(resolved):
            if resolved == bundle {
                return onPath ? .installed : .installedButNotOnPath
            }
            guard CLIInstall.isRunnyBundleCLI(resolved) else {
                return .conflict(owner: resolved)
            }
            // A link into some OTHER Runny.app. If that bundle is still on disk, install
            // adopts/re-points it (notInstalled). If it's gone, the link dangles — the
            // orphan a drag-to-trash leaves: surface it for cleanup, not a silent notInstalled.
            return targetExists ? .notInstalled : .orphaned(target: resolved)
        }
    }

    nonisolated static func stateForVerify(_ r: CLIInstall.VerifyResult) -> State {
        switch r {
        case .installed: .installed
        case .installedButNotOnPath: .installedButNotOnPath
        case .mismatch: .failed("the link now points somewhere else — a concurrent change; try again")
        case .missing: .failed("the install reported success but no link is there")
        }
    }

    /// Map osascript's exit + stderr to an outcome. `do shell script` surfaces the
    /// inner shell exit code in parentheses (our foreign guard exits 3), and a
    /// user-dismissed auth prompt is AppleScript error -128.
    nonisolated static func outcomeForOsascript(exitCode: Int32, stderr: String) -> PrivilegedOutcome {
        if exitCode == 0 { return .ok }
        // `do shell script` renders the inner shell exit (or AppleScript's own
        // error) as the FINAL parenthesized number: (-128) for a user-dismissed
        // prompt, (3) for our foreign-guard exit. Match the trailing token, not a
        // bare substring, so a path or message containing "(3)" earlier can't
        // misclassify a genuine failure as a benign foreign-refusal.
        let code = trailingParenCode(stderr)
        if code == -128 || stderr.localizedCaseInsensitiveContains("User canceled") {
            return .cancelled
        }
        if code == 3 { return .refusedForeign }
        return .failed(stderr.isEmpty ? "the privileged step failed (exit \(exitCode))" : stderr.trimmingCharacters(in: .whitespacesAndNewlines))
    }

    /// The final parenthesized integer in an osascript error string (e.g. "… (3)"
    /// → 3, "… (-128)" → -128), or nil if the message doesn't end in one.
    nonisolated static func trailingParenCode(_ s: String) -> Int? {
        let t = s.trimmingCharacters(in: .whitespacesAndNewlines)
        guard t.hasSuffix(")"), let open = t.lastIndex(of: "(") else { return nil }
        return Int(t[t.index(after: open) ..< t.index(before: t.endIndex)])
    }

    /// True when the bundle is translocated or at a transient path — a link into
    /// it would dangle on next launch. The Security SPI that answers this
    /// authoritatively isn't in the Swift import surface, so this matches the
    /// App Translocation mount root, which Gatekeeper always uses (no false
    /// negatives for the actual hazard) and fails closed for any /private/var
    /// transient path.
    nonisolated static func isTranslocated(_ bundlePath: String) -> Bool {
        bundlePath.contains("/AppTranslocation/") || bundlePath.hasPrefix("/private/var/folders/")
    }

    /// The install one-liner: create `/usr/local/bin` if missing, remove only a
    /// Runny.app link we own, refuse (exit 3) anything else still present, then
    /// create with a non-forcing `ln -s`. The guard re-runs at write time (closing
    /// the TOCTOU window a `brew install` between plan and write would open), and
    /// the non-forcing create fails loud on a late race rather than clobbering.
    nonisolated static func installScript(target: String) -> String {
        let sh = "mkdir -p /usr/local/bin && "
            + "existing=\"$(readlink /usr/local/bin/runnyctl 2>/dev/null)\"; "
            // Remove ONLY a link we own (this or another Runny.app). Then refuse if
            // anything still remains — a foreign file, or one that raced in.
            + "case \"$existing\" in */Runny.app/Contents/MacOS/runnyctl) rm -f /usr/local/bin/runnyctl ;; esac; "
            + "if [ -e /usr/local/bin/runnyctl ] || [ -L /usr/local/bin/runnyctl ]; then exit 3; fi; "
            // Non-forcing (no -f): a file racing in between the check and here makes
            // ln fail loudly rather than force-clobber a foreign runnyctl.
            + "ln -s \(shellSingleQuote(target)) /usr/local/bin/runnyctl"
        return appleScript(doShell: sh)
    }

    /// The uninstall one-liner: remove the link only when it points at THIS bundle
    /// (not merely some Runny.app — another copy's link is refused, matching
    /// plan(.uninstall), which returns removeOurs only for this bundle); test-and-
    /// remove at write time, refusing (exit 3) anything else still present.
    nonisolated static func removeScript(target: String) -> String {
        let sh = "existing=\"$(readlink /usr/local/bin/runnyctl 2>/dev/null)\"; "
            + "if [ \"$existing\" = \(shellSingleQuote(target)) ]; then rm -f /usr/local/bin/runnyctl; "
            + "elif [ -e /usr/local/bin/runnyctl ] || [ -L /usr/local/bin/runnyctl ]; then exit 3; fi"
        return appleScript(doShell: sh)
    }

    /// The orphan-cleanup one-liner: remove ONLY a Runny-owned link (this or any
    /// Runny.app) — the dangling leftover a removed app left behind — then refuse
    /// (exit 3) anything else still present. This is `installScript`'s remove+guard
    /// without the create; the `*/Runny.app/...` glob matches the orphan regardless of
    /// which (now-missing) bundle it pointed into, while a foreign file is never touched.
    nonisolated static func removeOrphanScript() -> String {
        let sh = "existing=\"$(readlink /usr/local/bin/runnyctl 2>/dev/null)\"; "
            + "case \"$existing\" in */Runny.app/Contents/MacOS/runnyctl) rm -f /usr/local/bin/runnyctl ;; esac; "
            + "if [ -e /usr/local/bin/runnyctl ] || [ -L /usr/local/bin/runnyctl ]; then exit 3; fi"
        return appleScript(doShell: sh)
    }

    /// Wrap a shell command as `do shell script "…" with administrator privileges`,
    /// escaping for the AppleScript string layer (backslash then double-quote).
    nonisolated static func appleScript(doShell sh: String) -> String {
        let escaped = sh
            .replacingOccurrences(of: "\\", with: "\\\\")
            .replacingOccurrences(of: "\"", with: "\\\"")
        return "do shell script \"\(escaped)\" with administrator privileges"
    }

    /// Single-quote a path for `/bin/sh`, escaping embedded single quotes the
    /// standard `'\''` way — so a path with spaces (or a quote) can't break out
    /// of the privileged command.
    nonisolated static func shellSingleQuote(_ s: String) -> String {
        "'" + s.replacingOccurrences(of: "'", with: "'\\''") + "'"
    }
}

// MARK: - Filesystem shell (impure, nonisolated)

extension CLIInstallModel {
    /// The raw state of a path: absent, a symlink (with its fully resolved
    /// target), or any other existing entry (a regular file → foreign).
    nonisolated static func readExisting(_ path: String) -> CLIInstall.Existing {
        let fm = FileManager.default
        guard let attrs = try? fm.attributesOfItem(atPath: path),
              let type = attrs[.type] as? FileAttributeType
        else { return .absent }
        if type == .typeSymbolicLink {
            return .symlink(resolvesTo: resolvedLink(path) ?? path)
        }
        return .regularFile
    }

    /// The link's target as an absolute path, or nil if nothing is there. For a
    /// live link the fully-resolved canonical path matches bundleCLIPath (also
    /// canonical). For a DANGLING link (target moved/deleted) resolvingSymlinksInPath
    /// gives back the link path itself, so fall back to the raw symlink destination
    /// (made absolute) — that still names the Runny.app a stale link points into, so
    /// it can be adopted/repaired rather than shown as an unfixable foreign conflict.
    nonisolated static func resolvedLink(_ path: String) -> String? {
        let fm = FileManager.default
        guard let attrs = try? fm.attributesOfItem(atPath: path) else { return nil }
        let resolved = URL(fileURLWithPath: path).resolvingSymlinksInPath().path
        if resolved != path { return resolved } // reached a live target
        // resolved == path → not a symlink, or a dangling one: read its destination.
        guard (attrs[.type] as? FileAttributeType) == .typeSymbolicLink,
              let dest = try? fm.destinationOfSymbolicLink(atPath: path)
        else { return resolved }
        if dest.hasPrefix("/") { return dest }
        return URL(fileURLWithPath: (path as NSString).deletingLastPathComponent)
            .appendingPathComponent(dest).standardized.path
    }

    /// Can the link be created without admin? True when the dir exists and is
    /// writable, or (the dir is absent) its parent is — covering the absent
    /// `/usr/local/bin` whose `/usr/local` the user owns.
    nonisolated static func dirWritable(_ dir: String) -> Bool {
        let fm = FileManager.default
        if fm.fileExists(atPath: dir) { return fm.isWritableFile(atPath: dir) }
        return fm.isWritableFile(atPath: (dir as NSString).deletingLastPathComponent)
    }

    /// Best-effort: is `dir` on the invoking environment's PATH? The app's PATH
    /// may differ from an interactive shell's, so a false here is a hint, not a
    /// certainty — surfaced as the distinct installedButNotOnPath state.
    nonisolated static func dirOnPath(_ dir: String) -> Bool {
        reachable(processPath: ProcessInfo.processInfo.environment["PATH"] ?? "",
                  systemPath: systemPath(), dir: dir)
    }

    /// Reachable in a fresh interactive shell iff `dir` is in EITHER the process
    /// PATH or the system path source. A Finder/login-item launch inherits
    /// launchd's PATH, which omits /usr/local/bin even though Terminal has it (via
    /// path_helper reading /etc/paths(.d)); checking only the process PATH would
    /// false-warn "not on PATH" on a normal install. The union avoids that while
    /// still catching the genuine /opt/homebrew-only case (in neither source).
    nonisolated static func reachable(processPath: String, systemPath: String, dir: String) -> Bool {
        pathContains(processPath, dir: dir) || pathContains(systemPath, dir: dir)
    }

    /// The path entries path_helper feeds every login shell: /etc/paths then each
    /// /etc/paths.d file, newline-separated, joined as a PATH string. Bounded file
    /// reads — no shell spawn, no side effects.
    nonisolated static func systemPath() -> String {
        var entries: [String] = []
        func add(_ contents: String) {
            entries += contents.split(whereSeparator: \.isNewline)
                .map { $0.trimmingCharacters(in: .whitespaces) }
                .filter { !$0.isEmpty }
        }
        if let s = try? String(contentsOfFile: "/etc/paths", encoding: .utf8) { add(s) }
        if let files = try? FileManager.default.contentsOfDirectory(atPath: "/etc/paths.d") {
            for f in files.sorted() where !f.hasPrefix(".") {
                if let s = try? String(contentsOfFile: "/etc/paths.d/\(f)", encoding: .utf8) { add(s) }
            }
        }
        return entries.joined(separator: ":")
    }

    /// Pure: is `dir` an element of a ":"-joined PATH, modulo a single trailing
    /// slash? `/usr/local/bin/` in PATH is the same directory as `/usr/local/bin`,
    /// and treating them as different would false-flag a reachable install.
    nonisolated static func pathContains(_ path: String, dir: String) -> Bool {
        func norm(_ s: Substring) -> Substring { s.count > 1 && s.hasSuffix("/") ? s.dropLast() : s }
        let want = norm(Substring(dir))
        return path.split(separator: ":").contains { norm($0) == want }
    }

    /// Create or re-point the link without escalation. Returns an error message
    /// on failure, nil on success. Re-point removes our prior link first
    /// (createSymbolicLink won't overwrite); the planner already proved it ours.
    nonisolated static func createUnprivileged(linkPath: String, target: String) -> String? {
        let fm = FileManager.default
        try? fm.createDirectory(atPath: (linkPath as NSString).deletingLastPathComponent,
                                withIntermediateDirectories: true)
        // Re-classify at the moment of write — the planner's read can be stale by a
        // concurrent install. Remove only an entry we still own (a Runny.app
        // symlink); never a foreign file that appeared. createSymbolicLink is
        // non-forcing, so a file racing in after this check fails loudly instead of
        // being clobbered.
        switch readExisting(linkPath) {
        case .absent:
            break
        case let .symlink(resolved) where CLIInstall.isRunnyBundleCLI(resolved):
            if (try? fm.removeItem(atPath: linkPath)) == nil {
                return "could not replace the existing runny link"
            }
        default:
            return "another runnyctl appeared at \(linkPath) — not replaced"
        }
        do {
            try fm.createSymbolicLink(atPath: linkPath, withDestinationPath: target)
            return nil
        } catch {
            return "could not create the link: \(error.localizedDescription)"
        }
    }

    nonisolated static func removeUnprivileged(_ linkPath: String, bundleCLIPath: String) -> String? {
        // Re-verify at write time: remove only a link that still resolves to THIS
        // bundle, never a foreign file that raced into the path.
        switch readExisting(linkPath) {
        case .absent:
            return nil
        case let .symlink(resolved) where resolved == bundleCLIPath:
            do {
                try FileManager.default.removeItem(atPath: linkPath)
                return nil
            } catch {
                return "could not remove the link: \(error.localizedDescription)"
            }
        default:
            return "another runnyctl now owns \(linkPath) — not removed"
        }
    }

    /// Remove a Runny-owned orphan link unprivileged. Like `removeUnprivileged` but
    /// matches ANY Runny.app link (the orphan points into a missing OTHER bundle, not
    /// this one), re-checked at write time so a foreign file that raced in is refused
    /// rather than removed.
    nonisolated static func removeOrphanUnprivileged(_ linkPath: String) -> String? {
        switch readExisting(linkPath) {
        case .absent:
            return nil
        case let .symlink(resolved) where CLIInstall.isRunnyBundleCLI(resolved):
            do {
                try FileManager.default.removeItem(atPath: linkPath)
                return nil
            } catch {
                return "could not remove the leftover link: \(error.localizedDescription)"
            }
        default:
            return "another runnyctl now owns \(linkPath) — not removed"
        }
    }
}
