import Foundation

/// The raw result of a bounded subprocess run, before any per-command
/// classification. Shared by `LaunchdProbe` (registration probe) and
/// `SMAppServiceRegistrar` (bootout/kickstart/print).
enum CommandResult: Equatable {
    case exited(code: Int32, stdout: String, stderr: String)
    case timedOut
    case launchFailed(String)
}

/// One bounded-subprocess scaffold for the app's `launchctl` surface: run a
/// process, drain its pipes (byte-capping BOTH streams), and on timeout send
/// SIGTERM, then SIGKILL after a grace, with a detached reaper and explicit
/// pipe-FD close — so a process that ignores SIGTERM never leaks a process or pipe
/// FDs. There is no `bounded.Context` in Swift; this is its GUI-side analog, and
/// the single home of the reaper/FD discipline so the two callers can't drift.
enum BoundedProcess {
    /// After SIGTERM on timeout, how long before SIGKILL. The caller is freed at
    /// `timeout`; this grace + the force-kill run detached so the zombie is reaped
    /// and the FDs closed even when SIGTERM is ignored.
    static let killGrace: Duration = .seconds(2)

    /// stderr is small for the callers here (a "could not find" line), but cap it
    /// anyway so a misbehaving binary can't stream unbounded stderr into memory —
    /// the same bound the stdout cap enforces.
    static let stderrByteCap = 64 * 1024

    static func run(
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
                DispatchQueue.global().asyncAfter(deadline: .now() + seconds(killGrace)) {
                    if proc.isRunning { kill(pid, SIGKILL) }
                }
                if gate.claim() { cont.resume(returning: .timedOut) }
            }
            DispatchQueue.global().asyncAfter(deadline: .now() + seconds(timeout), execute: killer)
            // The reader ALWAYS runs to EOF/exit, even after the caller was freed on
            // timeout — so the process is reaped and the FDs closed in every path.
            DispatchQueue.global().async {
                // Drain stdout (capped) BEFORE waitUntilExit so a verbose dump can't
                // deadlock on a full pipe buffer, then close the read end so a
                // still-writing process gets SIGPIPE and unblocks rather than wedging
                // the reaper; stderr is drained (capped) the same way.
                let outData = readCapped(outPipe.fileHandleForReading, cap: stdoutByteCap)
                try? outPipe.fileHandleForReading.close()
                let errData = readCapped(errPipe.fileHandleForReading, cap: stderrByteCap)
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

/// A one-shot gate so the timeout killer and the wait closure (different queues)
/// resume the continuation exactly once: whoever calls `claim()` first wins, the
/// loser's `claim()` returns false and does nothing. Self-contained rather than
/// pulling in Synchronization.Mutex for one flag.
final class ResumeOnce: @unchecked Sendable {
    private let lock = NSLock()
    private var claimed = false
    func claim() -> Bool {
        lock.lock()
        defer { lock.unlock() }
        if claimed { return false }
        claimed = true
        return true
    }
}
