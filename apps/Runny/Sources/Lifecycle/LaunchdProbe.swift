import Foundation

/// A bounded, single-label launchd registration probe. Answers one yes/no
/// question — "is `<label>` registered in this user's GUI domain?" — by running
/// `launchctl print gui/<getuid()>/<label>` under a hard wall-clock bound and
/// resolving registration by a **literal-label substring search in byte-capped
/// STDOUT** (the A1 finding), never by exit code and never by parsing the format.
///
/// Why stdout-only: a registered job (running OR stopped) prints a block whose
/// first line is `gui/<uid>/<label> = {`, so the literal label is always in
/// stdout — which is exactly why a `brew services stop runny` (registered but not
/// running) is still caught, where an exit-status read would miss it. An ABSENT
/// label yields empty stdout and echoes the label only in *stderr*
/// (`Could not find service "<label>"…`), so searching the combined stream would
/// false-positive on every absent label. The result feeds `DaemonOwnership`.
/// The launchd domain a label is probed in. `gui/<uid>` is the per-user agent
/// (Homebrew, the app's own, a manual install); `system` is a non-root system
/// daemon — the headless deployment. A non-root user can `launchctl print
/// system/<label>` and gets the same registered ("could not find" when absent)
/// signal as `gui`, so `classify` is shared across both.
enum LaunchdDomain: Sendable {
    case gui
    case system

    func target(_ label: String) -> String {
        switch self {
        case .gui: "gui/\(getuid())/\(label)"
        case .system: "system/\(label)"
        }
    }
}

enum LaunchdProbe {
    /// Hard wall-clock bound on one probe. A local `launchctl print` is sub-second;
    /// this is healthy-magnitude × margin, so a wedged launchctl yields
    /// `.indeterminate` (which `classify` treats as dominant) instead of spinning.
    static let timeout: Duration = .seconds(5)

    /// Read cap on captured stdout. A single-label dump is ~1.4 KB and the whole
    /// GUI domain ~122 KB; this is generous headroom that still bounds a
    /// pathological launchctl from streaming unbounded output into memory.
    static let stdoutByteCap = 64 * 1024

    /// Probe one label. `.registered` / `.notRegistered` / `.indeterminate`;
    /// the runner seam is injectable so the result mapping is unit-tested without
    /// shelling out.
    static func probe(
        label: String, domain: LaunchdDomain = .gui, runner: CommandRunner = ProcessCommandRunner()
    ) async -> LaunchdProbeResult {
        let result = await runner.run(
            "/bin/launchctl", ["print", domain.target(label)], timeout: timeout, stdoutByteCap: stdoutByteCap
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

/// The seam `LaunchdProbe` runs commands through, so the timeout/result mapping is
/// unit-tested against a fake while the real reaper/FD discipline lives below it.
protocol CommandRunner: Sendable {
    /// Run `executable args` under `timeout`, capturing up to `stdoutByteCap` bytes
    /// of stdout and all of (small) stderr. Never hangs past the bound: a process
    /// that ignores SIGTERM is SIGKILLed after a grace and reaped off the caller's
    /// path.
    func run(_ executable: String, _ args: [String], timeout: Duration, stdoutByteCap: Int) async -> CommandResult
}

/// The real `CommandRunner`: delegates to the shared `BoundedProcess` runner, so
/// the reaper/FD/byte-cap discipline lives in one place (shared with
/// `SMAppServiceRegistrar`) rather than a copy per caller.
struct ProcessCommandRunner: CommandRunner {
    func run(_ executable: String, _ args: [String], timeout: Duration, stdoutByteCap: Int) async -> CommandResult {
        await BoundedProcess.run(executable, args, timeout: timeout, stdoutByteCap: stdoutByteCap)
    }
}
