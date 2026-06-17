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

    /// How long to wait on a `launchctl bootout` before giving up loud. bootout is
    /// normally instant; the bound exists so a wedged launchctl surfaces a named
    /// failure instead of spinning the uninstall (no `bounded.Context` in Swift).
    static let bootoutTimeout: TimeInterval = 10

    private let service = SMAppService.agent(plistName: SMAppServiceRegistrar.plistName)

    func status() -> SMAppService.Status { service.status }

    func register() throws { try service.register() }

    func unregister() throws { try service.unregister() }

    func bootout() async -> BootoutOutcome {
        let target = "gui/\(getuid())/\(Self.agentLabel)"
        let proc = Process()
        proc.executableURL = URL(fileURLWithPath: "/bin/launchctl")
        proc.arguments = ["bootout", target]
        let errPipe = Pipe()
        proc.standardError = errPipe
        proc.standardOutput = Pipe()
        do {
            try proc.run()
        } catch {
            return .failed("could not launch launchctl bootout: \(error.localizedDescription)")
        }

        return await withCheckedContinuation { (cont: CheckedContinuation<BootoutOutcome, Never>) in
            // Bound the wait: if launchctl hasn't exited by bootoutTimeout, mark it
            // timed-out and terminate so waitUntilExit returns. The flag disambiguates
            // a natural exit from a terminate-on-timeout in the wait closure below.
            let timedOut = TimeoutFlag()
            let killer = DispatchWorkItem {
                timedOut.set()
                proc.terminate()
            }
            DispatchQueue.global().asyncAfter(deadline: .now() + Self.bootoutTimeout, execute: killer)
            DispatchQueue.global().async {
                proc.waitUntilExit()
                killer.cancel()
                if timedOut.isSet {
                    cont.resume(returning: .timedOut)
                    return
                }
                let data = errPipe.fileHandleForReading.readDataToEndOfFile()
                let stderr = String(data: data, encoding: .utf8) ?? ""
                cont.resume(returning: AgentController.classifyBootout(
                    exitCode: proc.terminationStatus, stderr: stderr
                ))
            }
        }
    }
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
