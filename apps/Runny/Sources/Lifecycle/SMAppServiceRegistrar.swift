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
        case let .exited(code, stderr):
            AgentController.classifyBootout(exitCode: code, stderr: stderr)
        case let .launchFailed(message):
            .failed(message)
        }
    }

    func kickstart() async throws {
        // No -k: kickstart starts a stopped job, never SIGKILLs a running one
        // (crash-only forbids interrupting a job). The daemon coming up is confirmed
        // from the connection by AgentController, not this call.
        switch await runLaunchctl(["kickstart", jobTarget]) {
        case .timedOut:
            throw LaunchctlFailure(message: "launchctl kickstart did not respond within \(Int(Self.launchctlTimeout))s")
        case let .exited(code, stderr) where code != 0:
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
        let errPipe = Pipe()
        proc.standardError = errPipe
        proc.standardOutput = Pipe()
        do {
            try proc.run()
        } catch {
            return .launchFailed("could not launch launchctl: \(error.localizedDescription)")
        }
        return await withCheckedContinuation { (cont: CheckedContinuation<LaunchctlResult, Never>) in
            // If launchctl hasn't exited by the bound, mark it timed-out and
            // terminate so waitUntilExit returns. The flag disambiguates a natural
            // exit from a terminate-on-timeout in the wait closure.
            let timedOut = TimeoutFlag()
            let killer = DispatchWorkItem {
                timedOut.set()
                proc.terminate()
            }
            DispatchQueue.global().asyncAfter(deadline: .now() + Self.launchctlTimeout, execute: killer)
            DispatchQueue.global().async {
                proc.waitUntilExit()
                killer.cancel()
                if timedOut.isSet {
                    cont.resume(returning: .timedOut)
                    return
                }
                let data = errPipe.fileHandleForReading.readDataToEndOfFile()
                let stderr = String(data: data, encoding: .utf8) ?? ""
                cont.resume(returning: .exited(code: proc.terminationStatus, stderr: stderr))
            }
        }
    }
}

/// The raw result of a bounded launchctl run, before per-verb classification.
private enum LaunchctlResult {
    case exited(code: Int32, stderr: String)
    case timedOut
    case launchFailed(String)
}

/// A launchctl verb failed or timed out. LocalizedError so the message reaches
/// AgentController's failure surface verbatim.
struct LaunchctlFailure: LocalizedError {
    let message: String
    var errorDescription: String? { message }
}

/// A tiny thread-safe bool: the timeout killer (one queue) and the wait closure
/// (another) race on it, so the read/write is lock-guarded. Self-contained rather
/// than pulling in Synchronization.Mutex for one flag.
private final class TimeoutFlag: @unchecked Sendable {
    private let lock = NSLock()
    private var value = false
    func set() { lock.withLock { value = true } }
    var isSet: Bool { lock.withLock { value } }
}
