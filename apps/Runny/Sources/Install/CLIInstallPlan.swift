import Foundation

/// The decision core for vending `runnyctl` as `/usr/local/bin/runnyctl`,
/// expressed as two pure functions over value types — no `FileManager`, no
/// `osascript`, no `Bundle`. The imperative shell (`CLIInstaller`) resolves the
/// real filesystem state into these inputs, applies the returned verdict, and
/// re-reads the disk to confirm; every branch here is unit-testable without
/// mutating `/usr/local/bin` or raising an admin prompt.
///
/// Two safety invariants live in this file, so neither can be lost in the
/// imperative wiring:
///  - **Never clobber a foreign file.** A regular file, or a symlink that does
///    not point into a Runny.app, is something another channel owns (a brew
///    `runnyctl`, a hand-rolled link). We refuse it; we never overwrite it.
///  - **Fail closed from a translocated bundle.** A link into a translocated
///    `/private/var/folders/.../Runny.app` evaporates on next launch. When the
///    caller cannot positively confirm a stable bundle path, install refuses
///    rather than create a doomed link.
enum CLIInstall {
    /// Whether the caller is installing/repairing the link or removing it. The
    /// same state classification drives both, but the safe action differs:
    /// install adopts or re-points a Runny-owned link onto this bundle; uninstall
    /// only removes a link that points into *this* bundle.
    enum Intent: Equatable {
        case install
        case uninstall
    }

    /// The raw state of `/usr/local/bin/runnyctl`, as the shell observes it.
    /// `symlink` carries the fully resolved (realpath) target so the pure
    /// classification — this bundle, another Runny.app, or foreign — happens
    /// here, under test, rather than in the shell.
    enum Existing: Equatable {
        case absent
        case symlink(resolvesTo: String)
        case regularFile
    }

    /// What the shell should do. The machine-readable axis the UI reads — never
    /// string-match a message to recover it (the same anti-re-parsing discipline
    /// the wire contract enforces elsewhere).
    enum Verdict: Equatable {
        /// Bundle is translocated / not at a stable path — refuse (fail closed).
        case refuseTranslocated
        /// The link already points at this bundle; nothing to do.
        case alreadyInstalled
        /// Create or re-point the link without escalation (the dir is writable).
        case createUnprivileged
        /// Create or re-point the link, but `/usr/local/bin` needs admin.
        case escalate
        /// A non-Runny file owns the path; never overwrite it.
        case refuseForeign
        /// Uninstall: the link points into this bundle; safe to remove.
        case removeOurs
        /// Uninstall: nothing at the path to remove.
        case notInstalled
    }

    /// The read-back result after a write, confirmed from disk — the UI flips to
    /// a success state only on this, never on the privileged step's exit code.
    enum VerifyResult: Equatable {
        /// The link resolves to this bundle and `/usr/local/bin` is on PATH.
        case installed
        /// The link resolves to this bundle but `/usr/local/bin` is not on PATH —
        /// "installed yet `runnyctl: command not found`", a loud state of its own.
        case installedButNotOnPath
        /// The link resolves somewhere else — a concurrent replacement.
        case mismatch
        /// No link, though the shell returned success.
        case missing
    }

    /// True when `path` is the `runnyctl` of *some* Runny.app bundle — at any
    /// location, so a moved or reinstalled app still reads as Runny-managed. The
    /// suffix is the whole bundle-relative path, not just the filename, so a
    /// foreign `…/runnyctl` cannot masquerade as ours.
    static func isRunnyBundleCLI(_ path: String) -> Bool {
        path.hasSuffix("/Runny.app/Contents/MacOS/runnyctl")
    }

    /// The install/uninstall decision. Pure: the caller supplies the resolved
    /// filesystem state and the two booleans it computed (is the bundle at a
    /// stable path, is `/usr/local/bin` writable without admin), and gets back
    /// exactly what to do.
    ///
    /// `bundleCLIPath` is this bundle's canonical `…/Contents/MacOS/runnyctl`.
    /// Translocation is checked first for install: a translocated bundle can
    /// never be safely linked regardless of what is already at the path.
    static func plan(
        intent: Intent,
        bundleCLIPath: String,
        existing: Existing,
        dirWritable: Bool,
        translocated: Bool
    ) -> Verdict {
        switch intent {
        case .install:
            if translocated { return .refuseTranslocated }
            switch classify(existing, bundleCLIPath: bundleCLIPath) {
            case .thisBundle:
                return .alreadyInstalled
            case .foreign:
                return .refuseForeign
            case .otherRunnyBundle, .nothing:
                // Absent, or a link into a different Runny.app we adopt onto this
                // bundle's path. Either way we (re-)create the link.
                return dirWritable ? .createUnprivileged : .escalate
            }
        case .uninstall:
            switch classify(existing, bundleCLIPath: bundleCLIPath) {
            case .thisBundle:
                return .removeOurs
            case .nothing:
                return .notInstalled
            case .otherRunnyBundle, .foreign:
                // Not ours to remove: a foreign file, or a link another Runny.app
                // copy installed. Leave it untouched.
                return .refuseForeign
            }
        }
    }

    /// The read-back verdict: compare the resolved link against this bundle and
    /// whether the dir is on PATH. `nil` resolvedLink means nothing is there.
    static func verify(
        bundleCLIPath: String,
        resolvedLink: String?,
        onPath: Bool
    ) -> VerifyResult {
        guard let resolved = resolvedLink else { return .missing }
        guard resolved == bundleCLIPath else { return .mismatch }
        return onPath ? .installed : .installedButNotOnPath
    }

    // MARK: - Classification (pure)

    private enum Ownership: Equatable {
        case nothing
        case thisBundle
        case otherRunnyBundle
        case foreign
    }

    private static func classify(_ existing: Existing, bundleCLIPath: String) -> Ownership {
        switch existing {
        case .absent:
            return .nothing
        case .regularFile:
            return .foreign
        case let .symlink(resolved):
            if resolved == bundleCLIPath { return .thisBundle }
            if isRunnyBundleCLI(resolved) { return .otherRunnyBundle }
            return .foreign
        }
    }
}
