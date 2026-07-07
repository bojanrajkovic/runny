import Foundation

/// A bounded, single-label launchd registration probe. Answers one yes/no
/// question — "is `<label>` registered in the `system/` domain?" — by running
/// `launchctl print system/<label>` under a hard wall-clock bound and resolving
/// registration by a **literal-label substring search in byte-capped STDOUT**
/// (the A1 finding), never by exit code and never by parsing the format.
///
/// Why stdout-only: a registered job (running OR stopped) prints a block whose
/// first line is `system/<label> = {`, so the literal label is always in
/// stdout — which is exactly why a registered-but-stopped daemon is still
/// caught, where an exit-status read would miss it. An ABSENT label yields empty
/// stdout and echoes the label only in *stderr*
/// (`Could not find service "<label>"…`), so searching the combined stream would
/// false-positive on every absent label.
///
/// The `system/` domain is the only one probed: it is the single foreign-detection
/// axis of `DaemonOwnership` — a registered `system/<label>` is the system daemon
/// that owns the shared socket the app dials. A non-root user CAN read the
/// `system/` domain and gets the same registered / "could not find" signal.
enum LaunchdProbe {
    /// Hard wall-clock bound on one probe. A local `launchctl print` is sub-second;
    /// this is healthy-magnitude × margin, so a wedged launchctl yields
    /// `.indeterminate` (which `classify` treats as dominant) instead of spinning.
    static let timeout: Duration = .seconds(5)

    /// Read cap on captured stdout. A single-label dump is ~1.4 KB; this is
    /// generous headroom that still bounds a pathological launchctl from
    /// streaming unbounded output into memory.
    static let stdoutByteCap = 64 * 1024

    /// Probe one label in the `system/` domain. `.registered` / `.notRegistered` /
    /// `.indeterminate`. Runs directly against the shared `BoundedProcess` runner
    /// (the reaper/FD/byte-cap discipline shared with `SMAppServiceRegistrar`);
    /// `classify` is the pure result mapping, unit-tested without shelling out.
    static func probe(label: String) async -> LaunchdProbeResult {
        let result = await BoundedProcess.run(
            "/bin/launchctl", ["print", "system/\(label)"], timeout: timeout, stdoutByteCap: stdoutByteCap
        )
        return Self.classify(result: result, label: label)
    }

    /// Map a raw command result to a registration verdict. A positive is the stable
    /// literal-label-in-stdout signal (catches running AND stopped); a clean absence
    /// is the recognized "could not find" in stderr; everything else — a timeout, a
    /// launch failure, or any other error (permission, SIP, a malformed selector) —
    /// is `.indeterminate`, so the app never classifies a daemon as unowned off a
    /// probe that merely failed.
    static func classify(result: CommandResult, label: String) -> LaunchdProbeResult {
        switch result {
        case .timedOut, .launchFailed:
            return .indeterminate
        case let .exited(_, stdout, stderr):
            if stdout.contains(label) { return .registered }
            if stderr.lowercased().contains("could not find") { return .notRegistered }
            return .indeterminate
        }
    }
}
