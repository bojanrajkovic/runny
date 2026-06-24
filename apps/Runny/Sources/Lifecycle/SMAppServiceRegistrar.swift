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
        // No -k: kickstart starts a stopped job, never SIGKILLs a running one —
        // a SIGKILL would take runnyd's in-process VMs, and the job running in
        // one, down with it. The daemon coming up is confirmed from the
        // connection by AgentController, not this call.
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

    /// Run a bounded `/bin/launchctl <args>` via the shared `BoundedProcess` runner
    /// (the SIGTERM→SIGKILL reaper, FD close, and byte caps live there, shared with
    /// `LaunchdProbe`). `bootout`/`kickstart`/`print` classify the `CommandResult`
    /// themselves. `print` is the only verbose verb; a 64 KB stdout cap comfortably
    /// contains the early `program = …` line `agentProgramPath` parses.
    private func runLaunchctl(_ args: [String]) async -> CommandResult {
        await BoundedProcess.run(
            "/bin/launchctl", args, timeout: .seconds(Self.launchctlTimeout), stdoutByteCap: 64 * 1024
        )
    }
}

/// A launchctl verb failed or timed out. LocalizedError so the message reaches
/// AgentController's failure surface verbatim.
struct LaunchctlFailure: LocalizedError {
    let message: String
    var errorDescription: String? { message }
}
