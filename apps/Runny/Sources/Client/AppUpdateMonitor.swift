import Foundation

/// The app-update notify poll: fetches GitHub releases/latest (fail-quiet — every
/// error path is silence, never a false "you're behind" banner) on launch, then
/// every 24h, and on the "Check for Updates…" menu command
/// (`.runnyCheckForAppUpdates`). Split out of `DaemonStore` because it shares no
/// state with the connection FSM — only `DaemonStore.appVersion`, a constant, and
/// `AppUpdateChecker`, already its own type. `DaemonStore` holds one instance;
/// views reach it directly via `store.updateMonitor`.
@MainActor
@Observable
final class AppUpdateMonitor {
    /// The latest Runny release newer than this app's stamped version, or nil
    /// when the app is current, no check has run yet, or the check failed.
    /// Set by the 24h timer + launch + manual "Check for Updates…" check.
    private(set) var availableUpdate: DaemonStore.AppUpdate?
    /// The update the operator dismissed — keyed on the version string so a
    /// re-check of the same release stays quiet, but a newer release is new news.
    var dismissedUpdate: DaemonStore.AppUpdate?
    /// The banner to show: `availableUpdate` minus what was dismissed. A
    /// dismissed "v0.7.0" stays gone until a "v0.7.1" check arrives.
    var shownUpdate: DaemonStore.AppUpdate? {
        availableUpdate.flatMap { $0.version == dismissedUpdate?.version ? nil : $0 }
    }

    private var checkTask: Task<Void, Never>?
    private var checkForUpdatesObserver: NSObjectProtocol?

    /// App-lifetime: registers the "Check for Updates…" observer and starts the
    /// 24h loop once. Idempotent — safe to call on every `DaemonStore.start()`,
    /// including after a `restart()` (this is NOT connection-scoped).
    func start() {
        guard checkTask == nil else { return }
        checkForUpdatesObserver = NotificationCenter.default.addObserver(
            forName: .runnyCheckForAppUpdates, object: nil, queue: .main
        ) { [weak self] _ in
            Task { @MainActor in await self?.runCheck() }
        }
        checkTask = Task { await checkLoop() }
    }

    private func checkLoop() async {
        await runCheck()
        while !Task.isCancelled {
            try? await Task.sleep(for: .seconds(86400))
            if Task.isCancelled { return }
            await runCheck()
        }
    }

    private func runCheck() async {
        let enabled = UserDefaults.standard.object(forKey: Prefs.checkForAppUpdates) as? Bool ?? true
        guard enabled else { return }
        if let result = await AppUpdateChecker.fetch(appVersion: DaemonStore.appVersion) {
            availableUpdate = result
        }
    }
}
