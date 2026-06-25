import Foundation
import Observation

/// Installs and removes the non-root system LaunchDaemon from the app, via ONE
/// `with administrator privileges` prompt brokered through the shared
/// `PrivilegedBroker`. The brokered command stages the bundled `runnyd`+`runnyctl`
/// to a stable location OUTSIDE the app bundle (`/usr/local/libexec/runny`, the
/// same target the headless `install-system.sh` uses) so the system daemon survives
/// the app being deleted, then runs the staged `runnyctl install-daemon` — the exact
/// privileged installer the CLI path uses, so the app and a headless host produce
/// the identical on-disk shape (dedicated `_runny` account, dual-ACL home, system
/// plist, `launchctl bootstrap system`).
///
/// The success signal is the launchd `system/` job being REGISTERED, NOT the daemon
/// answering its socket: `install-daemon` leaves the daemon crash-looping until a
/// config is in place (same as the headless path), so "answers the socket" would
/// read a correct install as a failure. The decision logic — the brokered scripts,
/// the osascript-result mapping, the probe→state confirmation — is the pure surface
/// below; this file owns only the bundle resolution and the broker call that can't
/// run under test.
@MainActor
@Observable
final class SystemDaemonInstaller {
    enum State: Equatable {
        /// No system daemon is registered — install is offered.
        case notInstalled
        /// A brokered op is in flight; the UI shows a cancel.
        case installing
        /// The launchd `system/` job is registered. It may still be crash-looping
        /// until a config is written to the system home — a separate, surfaced step.
        case installed
        /// The app is translocated / at a transient path; refused (fail closed) — a
        /// daemon staged from a bundle that evaporates on next launch is a trap.
        case translocated
        case failed(String)
        case cancelled
    }

    private(set) var state: State = .notInstalled

    /// Monotonic op identity. A cancel bumps it; a late async result whose captured
    /// generation is stale is ignored, so a cancelled op can never overwrite the loud
    /// `.cancelled` state.
    private var generation = 0
    /// The shared admin-prompt runner — owns the live process so `cancel()` can
    /// terminate a standing prompt.
    private let broker = PrivilegedBroker()

    /// The launchd `system/` registration probe — the confirm-from-disk signal,
    /// injected so the state machine is exercised with fakes without shelling out to
    /// `launchctl`. The same probe `AgentController` uses, so the installer's verdict
    /// and the app's ownership detection agree on "a system daemon is present".
    private let systemProbe: @Sendable () async -> LaunchdProbeResult

    init(
        systemProbe: @escaping @Sendable () async -> LaunchdProbeResult = {
            await LaunchdProbe.probe(label: DaemonOwnership.canonicalLabel)
        }
    ) {
        self.systemProbe = systemProbe
    }

    static let libexecDir = "/usr/local/libexec/runny"

    /// This bundle's nested `runnyd`/`runnyctl`. nil for an unbundled dev build (a
    /// bare `bazel run` carries no `Contents/MacOS/<bin>`), where install is simply
    /// unavailable rather than wrong.
    static var bundleRunnydPath: String? { bundleBinary("runnyd", in: Bundle.main.bundleURL) }
    static var bundleRunnyctlPath: String? { bundleBinary("runnyctl", in: Bundle.main.bundleURL) }

    private static func bundleBinary(_ name: String, in bundleURL: URL) -> String? {
        let url = bundleURL.appendingPathComponent("Contents/MacOS/\(name)")
        return FileManager.default.fileExists(atPath: url.path) ? url.path : nil
    }

    /// Recompute the resting state from the launchd `system/` probe — on appear. A
    /// no-op while an op is in flight (overwriting `.installing` mid-prompt would hide
    /// Cancel). An indeterminate probe leaves `.notInstalled` (install-daemon is the
    /// idempotent recovery), never a false `.installed`.
    func refresh() async {
        guard state != .installing else { return }
        state = await systemProbe() == .registered ? .installed : .notInstalled
    }

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

    /// Cancel an in-flight op: terminate the privileged process if one is up and flip
    /// to the loud terminal `.cancelled`. The generation bump makes any late result a
    /// no-op.
    func cancel() {
        guard state == .installing else { return }
        generation += 1
        broker.cancel()
        state = .cancelled
    }

    // MARK: - Imperative shell (live-machine only)

    private func runInstall(gen: Int) async {
        // Read the bundle location ONCE and derive both the translocation guard and
        // the staged source from it, so the "never stage from a bundle that
        // evaporates" invariant can't be split across two independent reads.
        let bundleURL = Bundle.main.bundleURL
        guard let runnyd = Self.bundleBinary("runnyd", in: bundleURL),
              let runnyctl = Self.bundleBinary("runnyctl", in: bundleURL)
        else {
            apply(.failed("this build carries no bundled runnyd/runnyctl to install"), gen: gen)
            return
        }
        // Refuse a translocated / transient bundle: the staged copy would be of a
        // bundle that evaporates on next launch, and a system daemon must outlive it.
        if PrivilegedBroker.isTranslocated(bundleURL.path) {
            apply(.translocated, gen: gen)
            return
        }
        let script = Self.installScript(bundleRunnyd: runnyd, bundleRunnyctl: runnyctl, operator: NSUserName())
        guard gen == generation else { return } // cancelled before the prompt
        let result = await broker.run(script)
        guard gen == generation else { return } // cancel() already set .cancelled
        switch Self.outcome(for: result) {
        case .ok:
            await confirm(gen: gen, map: Self.stateForInstallProbe)
        case .cancelled:
            apply(.cancelled, gen: gen)
        case let .failed(message):
            apply(.failed(message), gen: gen)
        }
    }

    private func runUninstall(gen: Int) async {
        guard let runnyctl = Self.bundleRunnyctlPath else {
            apply(.failed("this build carries no bundled runnyctl to run the uninstall"), gen: gen)
            return
        }
        guard gen == generation else { return }
        let result = await broker.run(Self.uninstallScript(bundleRunnyctl: runnyctl))
        guard gen == generation else { return }
        switch Self.outcome(for: result) {
        case .ok:
            await confirm(gen: gen, map: Self.stateForUninstallProbe)
        case .cancelled:
            apply(.cancelled, gen: gen)
        case let .failed(message):
            apply(.failed(message), gen: gen)
        }
    }

    /// Confirm the effect from the launchd `system/` probe — the requested≠done
    /// invariant: the UI flips to a terminal state only on what launchd actually
    /// shows, never on the privileged step's exit code.
    private func confirm(gen: Int, map: @Sendable (LaunchdProbeResult) -> State) async {
        let probe = await systemProbe()
        apply(map(probe), gen: gen)
    }

    /// Set state only if this op is still current — a cancelled/superseded op's late
    /// result is dropped.
    private func apply(_ new: State, gen: Int) {
        guard gen == generation else { return }
        state = new
    }
}

// MARK: - Pure surface (nonisolated static, unit-tested)

extension SystemDaemonInstaller {
    /// The brokered step's outcome, recovered from the broker's run result — the
    /// daemon installer's DOMAIN reading (unlike the CLI's, there is no foreign-file
    /// exit-3 branch: any non-cancel nonzero is a failure carrying install-daemon's
    /// own stderr).
    enum Outcome: Equatable {
        case ok
        case cancelled
        case failed(String)
    }

    nonisolated static func outcome(for result: PrivilegedBroker.RunResult) -> Outcome {
        switch result {
        case let .launchFailed(message):
            return .failed(message)
        case let .completed(exitCode, stderr):
            if exitCode == 0 { return .ok }
            if PrivilegedBroker.isUserCancelled(exitCode: exitCode, stderr: stderr) { return .cancelled }
            return .failed(stderr.isEmpty
                ? "the privileged install failed (exit \(exitCode))"
                : stderr.trimmingCharacters(in: .whitespacesAndNewlines))
        }
    }

    /// The brokered install one-liner, run once as root. Stages the bundled binaries
    /// to `libexecDir` (a stable path outside the bundle, so the daemon survives the
    /// app's deletion) and runs the STAGED runnyctl's install-daemon — so it resolves
    /// the STAGED runnyd as its sibling and the plist pins the stable path. `rm -f`
    /// before `cp` keeps a re-stage over a running daemon's binary from hitting
    /// ETXTBSY (the old inode lives until the daemon exits; install-daemon's bootstrap
    /// restarts onto the new one). The bundle paths and the operator are single-quoted,
    /// so a space or metacharacter can't break out of the root command; the operator
    /// is additionally validated by `runnyctl` itself before any privileged step.
    nonisolated static func installScript(bundleRunnyd: String, bundleRunnyctl: String, operator op: String) -> String {
        let runnyd = PrivilegedBroker.shellSingleQuote(bundleRunnyd)
        let runnyctl = PrivilegedBroker.shellSingleQuote(bundleRunnyctl)
        let operatorArg = PrivilegedBroker.shellSingleQuote(op)
        let stagedRunnyd = "\(libexecDir)/runnyd"
        let stagedRunnyctl = "\(libexecDir)/runnyctl"
        let sh = "mkdir -p \(libexecDir) && "
            + "rm -f \(stagedRunnyd) \(stagedRunnyctl) && "
            + "cp \(runnyd) \(stagedRunnyd) && cp \(runnyctl) \(stagedRunnyctl) && "
            + "chmod 0755 \(stagedRunnyd) \(stagedRunnyctl) && "
            + "\(stagedRunnyctl) install-daemon --operator \(operatorArg)"
        return PrivilegedBroker.appleScript(doShell: sh)
    }

    /// The brokered uninstall one-liner: the BUNDLE's runnyctl uninstall-daemon (boots
    /// out the job, removes the plist AND the system home). Uses the bundle copy, not
    /// the staged one, so uninstall works even if `libexecDir` was cleaned;
    /// uninstall-daemon needs no runnyd sibling, so the bundle path suffices.
    nonisolated static func uninstallScript(bundleRunnyctl: String) -> String {
        PrivilegedBroker.appleScript(doShell: "\(PrivilegedBroker.shellSingleQuote(bundleRunnyctl)) uninstall-daemon")
    }

    /// Confirm an install from the launchd `system/` probe. Registered = the bootstrap
    /// took (the install succeeded even if the daemon then crash-loops awaiting
    /// config). Anything else after a success exit is a loud failure, never a false
    /// `.installed`.
    nonisolated static func stateForInstallProbe(_ probe: LaunchdProbeResult) -> State {
        switch probe {
        case .registered:
            .installed
        case .notRegistered:
            .failed("the install reported success but no system daemon is registered "
                + "(check `launchctl print system/\(DaemonOwnership.canonicalLabel)`)")
        case .indeterminate:
            .failed("couldn't confirm the system daemon registered "
                + "(check `launchctl print system/\(DaemonOwnership.canonicalLabel)`)")
        }
    }

    /// Confirm an uninstall from the launchd `system/` probe — the mirror: gone =
    /// removed; still registered after a success exit means it did not take.
    nonisolated static func stateForUninstallProbe(_ probe: LaunchdProbeResult) -> State {
        switch probe {
        case .notRegistered:
            .notInstalled
        case .registered:
            .failed("the uninstall reported success but the system daemon is still registered")
        case .indeterminate:
            .failed("couldn't confirm the system daemon was removed "
                + "(check `launchctl print system/\(DaemonOwnership.canonicalLabel)`)")
        }
    }
}
