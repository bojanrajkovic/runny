import AppKit
import SwiftUI

@main
struct RunnyApp: App {
    @State private var store = DaemonStore()
    @State private var activation = ActivationCoordinator()
    @State private var agent = AgentController.live()

    var body: some Scene {
        MenuBarExtra("Runny", image: "MenuBarIcon") {
            MenuBarView()
                .environment(store)
                .environment(activation)
                .environment(agent)
        }
        .menuBarExtraStyle(.window)

        Window("Runny", id: WindowID.main) {
            MainWindowView()
                .environment(store)
                .environment(activation)
                .environment(agent)
                .onAppear {
                    store.start()
                    activation.windowAppeared()
                    // The agent refresh + reconcile (and the auto-apply that follows)
                    // run via `.autoApplyOnAppear()` on MainWindowView, shared with the
                    // popover — so they aren't duplicated here.
                }
        }
        .defaultSize(width: 920, height: 600)
        // Standard title bar: each detail pane sets its own `navigationTitle`
        // (the runner name / "Doctor" / "Daemon log"), so the bar shows the pane
        // title, never a stranded "Runny". The title bar must stay visible —
        // `.hiddenTitleBar` suppresses `navigationTitle`, which forced the title
        // into a glass-capsuled principal toolbar item that read as a button.
        .commands {
            // "Check for Updates…" in the app menu. Posts a notification that
            // DaemonStore's observer picks up and runs an immediate check,
            // bypassing the 24h timer. NotificationCenter avoids threading the
            // store through the commands scene (which has no @Environment path).
            CommandGroup(after: .appInfo) {
                Button("Check for Updates…") {
                    NotificationCenter.default.post(
                        name: .runnyCheckForAppUpdates, object: nil
                    )
                }
            }
        }

        // The Settings surface (the per-user daemon's start-at-login row). Its
        // onAppear registers the same accessory↔regular observer the main window
        // does, so closing it never strands the app.
        Settings {
            SettingsView()
                .environment(store)
                .environment(agent)
                .environment(activation)
        }
    }

    init() {
        // The "Runny home" Settings override was retired when the home was fixed
        // at ~/.runny; drop any value a prior version left in defaults. Idempotent
        // (removing an absent key is a no-op) — safe to run on every launch.
        UserDefaults.standard.removeObject(forKey: "runnyHomeOverride")
        // LSUIElement keeps us out of the Dock at launch; the coordinator
        // flips to .regular while the main window is open.
        NSApp?.setActivationPolicy(.accessory)
    }
}

enum WindowID {
    static let main = "main"
}

/// Flips activation policy .accessory ↔ .regular around the main window.
///
/// SwiftUI Window scenes have no close callback, so closure is observed via
/// NSWindow.willCloseNotification filtered by identifier — onDisappear of the
/// root view is not a reliable scene-close signal. We only drop back to
/// .accessory when no other regular window (e.g. Settings) remains visible.
@MainActor
@Observable
final class ActivationCoordinator {
    private var closeObserver: NSObjectProtocol?

    /// Open the main window from the popover: policy first, then activate
    /// after a main-queue hop — otherwise the window opens behind the
    /// frontmost app without key focus.
    func openMainWindow(_ openWindow: OpenWindowAction) {
        NSApp.setActivationPolicy(.regular)
        openWindow(id: WindowID.main)
        DispatchQueue.main.async {
            NSApp.activate(ignoringOtherApps: true)
        }
        dismissMenuBarPanel()
    }

    /// Called when the main window OR Settings appears: go `.regular` and register
    /// the close observer once. The observer reverts to `.accessory` only when no
    /// regular, key-able, non-panel window remains, so whichever of the two
    /// outlives the other keeps the app visible until both are gone.
    func windowAppeared() {
        NSApp.setActivationPolicy(.regular)
        guard closeObserver == nil else { return }
        closeObserver = NotificationCenter.default.addObserver(
            forName: NSWindow.willCloseNotification, object: nil, queue: .main
        ) { notification in
            // Fire on ANY window close, not just the main window's, and revert
            // only when no other regular, key-able, non-panel window remains.
            // The main window is currently the only such window, but the check
            // stays general: a second window that outlives the main one (e.g. a
            // future Settings scene) must not strand the app .regular with
            // nothing visible.
            guard let closing = notification.object as? NSWindow else { return }
            Task { @MainActor in
                let remaining = NSApp.windows.contains { window in
                    window !== closing && window.isVisible
                        && window.canBecomeKey && !(window is NSPanel)
                }
                if !remaining {
                    NSApp.setActivationPolicy(.accessory)
                }
            }
        }
    }

    /// MenuBarExtra(.window) has no public programmatic dismissal —
    /// @Environment(\.dismiss) is a no-op inside it. Closing the hosting
    /// panel directly is the minimal shim (vs. a third-party package).
    /// Sharp edge documented in apps/Runny/CLAUDE.md; revisit each macOS.
    func dismissMenuBarPanel() {
        for window in NSApp.windows
            where window.className.contains("MenuBarExtraWindow")
        {
            window.close()
        }
    }
}
