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
    static func probe(label: String, runner: CommandRunner = ProcessCommandRunner()) async -> LaunchdProbeResult {
        let target = "gui/\(getuid())/\(label)"
        let result = await runner.run(
            "/bin/launchctl", ["print", target], timeout: timeout, stdoutByteCap: stdoutByteCap
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

/// The raw result of a bounded command run, before per-probe classification.
enum CommandResult: Equatable {
    case exited(code: Int32, stdout: String, stderr: String)
    case timedOut
    case launchFailed(String)
}

/// The real `CommandRunner`: the one impure piece. Runs the process, drains the
/// pipes (byte-capping stdout), and on timeout sends SIGTERM, then SIGKILL after a
/// grace, with a detached reaper so a SIGTERM-ignoring launchctl never leaks a
/// process or pipe FDs — the bounded-operation discipline applied to the GUI.
struct ProcessCommandRunner: CommandRunner {
    /// After SIGTERM on timeout, how long to wait before SIGKILL. The caller is
    /// already freed at `timeout`; this grace + the force-kill run detached so the
    /// zombie is reaped and the FDs closed even when SIGTERM is ignored.
    static let killGrace: Duration = .seconds(2)

    func run(
        _ executable: String, _ args: [String], timeout: Duration, stdoutByteCap: Int
    ) async -> CommandResult {
        let proc = Process()
        proc.executableURL = URL(fileURLWithPath: executable)
        proc.arguments = args
        let outPipe = Pipe()
        let errPipe = Pipe()
        proc.standardOutput = outPipe
        proc.standardError = errPipe
        do {
            try proc.run()
        } catch {
            return .launchFailed("could not launch \(executable): \(error.localizedDescription)")
        }
        return await withCheckedContinuation { (cont: CheckedContinuation<CommandResult, Never>) in
            // Resume the caller at `timeout` regardless of whether the process has
            // exited; the gate makes a clean exit a hair before the deadline win the
            // race so it is reported correctly rather than as a false `.timedOut`.
            let gate = ResumeOnce()
            let killer = DispatchWorkItem {
                proc.terminate() // SIGTERM, best-effort
                let pid = proc.processIdentifier
                // Force-kill if SIGTERM was ignored, so the reader's waitUntilExit
                // returns and the process/FDs are reaped instead of leaking.
                DispatchQueue.global().asyncAfter(deadline: .now() + Self.seconds(Self.killGrace)) {
                    if proc.isRunning { kill(pid, SIGKILL) }
                }
                if gate.claim() { cont.resume(returning: .timedOut) }
            }
            DispatchQueue.global().asyncAfter(deadline: .now() + Self.seconds(timeout), execute: killer)
            // The reader ALWAYS runs to EOF/exit, even after the caller was freed on
            // timeout — so the process is reaped and the FDs closed in every path.
            DispatchQueue.global().async {
                // Drain stdout (capped) BEFORE waitUntilExit so a verbose dump can't
                // deadlock on a full pipe buffer, then close the read end so a
                // still-writing process gets SIGPIPE and unblocks rather than wedging
                // the reaper. stderr is small (the "could not find" line), read whole.
                let outData = Self.readCapped(outPipe.fileHandleForReading, cap: stdoutByteCap)
                try? outPipe.fileHandleForReading.close()
                let errData = errPipe.fileHandleForReading.readDataToEndOfFile()
                try? errPipe.fileHandleForReading.close()
                proc.waitUntilExit()
                killer.cancel()
                if gate.claim() {
                    cont.resume(returning: .exited(
                        code: proc.terminationStatus,
                        stdout: String(data: outData, encoding: .utf8) ?? "",
                        stderr: String(data: errData, encoding: .utf8) ?? ""
                    ))
                }
            }
        }
    }

    /// Read up to `cap` bytes, then stop — bounding memory against a process that
    /// streams unbounded output. The caller closes the handle right after, so the
    /// writer gets SIGPIPE instead of blocking forever on a pipe we stopped draining.
    private static func readCapped(_ handle: FileHandle, cap: Int) -> Data {
        var data = Data()
        while data.count < cap {
            let chunk = handle.readData(ofLength: min(64 * 1024, cap - data.count))
            if chunk.isEmpty { break } // EOF
            data.append(chunk)
        }
        return data
    }

    private static func seconds(_ duration: Duration) -> Double {
        let c = duration.components
        return Double(c.seconds) + Double(c.attoseconds) / 1e18
    }
}
