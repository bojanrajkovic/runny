import Foundation
import ServiceManagement

/// The only place `SMAppService.agent` and `launchctl` are touched. Everything
/// here is the side effect `ServiceRegistrar` abstracts away from the testable
/// `AgentController` decisions; nothing here is unit-tested live.
@MainActor
final class SMAppServiceRegistrar: ServiceRegistrar {
    /// The agent's launchd label, used for the `launchctl bootout` target. Must
    /// match the bundled plist's `Label`.
    static let agentLabel = "com.coderinserepeat.runnyd"
    /// The bundled plist's filename. `SMAppService.agent(plistName:)` resolves it
    /// against the registering bundle's `Contents/Library/LaunchAgents/`.
    static let plistName = "com.coderinserepeat.runnyd.plist"

    /// How long to wait on a `launchctl` verb (bootout, kickstart) before giving up
    /// loud. Both are normally instant; the bound exists so a wedged launchctl
    /// surfaces a named failure instead of spinning (no `bounded.Context` in Swift).
    static let launchctlTimeout: TimeInterval = 10

    private let service = SMAppService.agent(plistName: SMAppServiceRegistrar.plistName)

    func status() -> SMAppService.Status { service.status }

    func register() throws { try service.register() }

    func unregister() throws { try service.unregister() }

    private var jobTarget: String { "gui/\(getuid())/\(Self.agentLabel)" }

    func bootout() async -> BootoutOutcome {
        switch await runLaunchctl(["bootout", jobTarget]) {
        case .timedOut:
            .timedOut
        case let .exited(code, _, stderr):
            AgentController.classifyBootout(exitCode: code, stderr: stderr)
        case let .launchFailed(message):
            .failed(message)
        }
    }

    func agentProgramPath() async -> AgentProgram {
        switch await runLaunchctl(["print", jobTarget]) {
        case .timedOut, .launchFailed:
            return .undetermined
        case let .exited(code, stdout, stderr):
            if code != 0 {
                // "Could not find service" = the job genuinely isn't loaded. Any
                // OTHER non-zero (a permission error on a managed host, a corrupt
                // service DB) must NOT masquerade as not-registered — that would
                // mark reconcile .ok and hide a foreign/wedged agent. Surface it as
                // undetermined instead.
                return stderr.lowercased().contains("could not find") ? .notRegistered : .undetermined
            }
            // Loaded: pull the program path; an unparseable dump is undetermined,
            // never a false "foreign".
            if let program = AgentController.parseLaunchctlProgram(stdout) {
                return .program(program)
            }
            return .undetermined
        }
    }

    func kickstart() async throws {
        // No -k: kickstart starts a stopped job, never SIGKILLs a running one
        // (crash-only forbids interrupting a job). The daemon coming up is confirmed
        // from the connection by AgentController, not this call.
        switch await runLaunchctl(["kickstart", jobTarget]) {
        case .timedOut:
            throw LaunchctlFailure(message: "launchctl kickstart did not respond within \(Int(Self.launchctlTimeout))s")
        case let .exited(code, _, stderr) where code != 0:
            throw LaunchctlFailure(message: stderr.isEmpty
                ? "launchctl kickstart exited \(code)"
                : stderr.trimmingCharacters(in: .whitespacesAndNewlines))
        case .exited:
            return
        case let .launchFailed(message):
            throw LaunchctlFailure(message: message)
        }
    }

    /// The bounded launchctl subprocess scaffold both bootout and kickstart share:
    /// run `/bin/launchctl <args>`, wait off the main actor, and if it hasn't
    /// exited by `launchctlTimeout`, terminate it and report `.timedOut` — so a
    /// wedged launchctl surfaces a named result instead of spinning (no
    /// `bounded.Context` in Swift). Extracted because duplicating the timeout dance
    /// across two call sites would risk one drifting from the other.
    private func runLaunchctl(_ args: [String]) async -> LaunchctlResult {
        let proc = Process()
        proc.executableURL = URL(fileURLWithPath: "/bin/launchctl")
        proc.arguments = args
        let outPipe = Pipe()
        let errPipe = Pipe()
        proc.standardOutput = outPipe
        proc.standardError = errPipe
        do {
            try proc.run()
        } catch {
            return .launchFailed("could not launch launchctl: \(error.localizedDescription)")
        }
        return await withCheckedContinuation { (cont: CheckedContinuation<LaunchctlResult, Never>) in
            // Resume the CALLER on the timeout regardless of whether the process has
            // exited — so a launchctl that ignores SIGTERM can never hang the caller
            // (the wait closure may leak a thread in that pathological case, but the
            // bound is honored). The resume-once gate also resolves the natural race:
            // a clean exit a hair before the deadline resumes `.exited` first, so it
            // is reported correctly rather than as a false `.timedOut`.
            let gate = ResumeOnce()
            let killer = DispatchWorkItem {
                proc.terminate() // best-effort SIGTERM; the caller is freed either way
                if gate.claim() { cont.resume(returning: .timedOut) }
            }
            DispatchQueue.global().asyncAfter(deadline: .now() + Self.launchctlTimeout, execute: killer)
            DispatchQueue.global().async {
                // Drain both pipes BEFORE waitUntilExit: a verbose `launchctl print`
                // can fill the pipe buffer and deadlock a process that blocks on
                // write while we wait on exit. Reading to EOF returns when the
                // process closes its ends (on exit or terminate).
                let outData = outPipe.fileHandleForReading.readDataToEndOfFile()
                let errData = errPipe.fileHandleForReading.readDataToEndOfFile()
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
}

/// The raw result of a bounded launchctl run, before per-verb classification.
private enum LaunchctlResult {
    case exited(code: Int32, stdout: String, stderr: String)
    case timedOut
    case launchFailed(String)
}

/// A launchctl verb failed or timed out. LocalizedError so the message reaches
/// AgentController's failure surface verbatim.
struct LaunchctlFailure: LocalizedError {
    let message: String
    var errorDescription: String? { message }
}

/// A one-shot gate so the timeout killer and the wait closure (different queues)
/// resume the continuation exactly once: whoever calls `claim()` first wins, the
/// loser's `claim()` returns false and it does nothing. Self-contained rather than
/// pulling in Synchronization.Mutex for one flag.
private final class ResumeOnce: @unchecked Sendable {
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
