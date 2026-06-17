import AppKit
import SwiftUI

@main
struct RunnyApp: App {
    @State private var store = DaemonStore()
    @State private var activation = ActivationCoordinator()

    var body: some Scene {
        MenuBarExtra("Runny", image: "MenuBarIcon") {
            MenuBarView()
                .environment(store)
                .environment(activation)
        }
        .menuBarExtraStyle(.window)

        Window("Runny", id: WindowID.main) {
            MainWindowView()
                .environment(store)
                .environment(activation)
                .onAppear {
                    store.start()
                    activation.mainWindowAppeared()
                }
        }
        .defaultSize(width: 920, height: 600)
        // Drop the "Runny" titlebar text: above the big in-content runner name
        // it reads as a stranded second title. The toolbar (traffic lights,
        // sidebar toggle) stays; the content header is the sole title.
        .windowStyle(.hiddenTitleBar)

        Settings {
            SettingsView()
                .environment(store)
        }
    }

    init() {
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

    func mainWindowAppeared() {
        NSApp.setActivationPolicy(.regular)
        guard closeObserver == nil else { return }
        closeObserver = NotificationCenter.default.addObserver(
            forName: NSWindow.willCloseNotification, object: nil, queue: .main
        ) { notification in
            // Fire on ANY window close, not just the main window's: if
            // Settings outlives the main window, gating on the main-window
            // identifier ignored its close and left the app .regular with no
            // windows. Revert whenever the last regular window goes (Settings
            // counts — it's a regular, key-able, non-panel window).
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

struct SettingsView: View {
    var body: some View {
        Form {
            Section {
                // The home is fixed at ~/.runny with no override, so the socket
                // path is read-only — shown for diagnostics only. The manual
                // Reconnect affordance lives on the main-window daemon card.
                LabeledContent("Socket", value: RunnyHome.displaySocketPath)
            }
        }
        .formStyle(.grouped)
        .frame(width: 480)
    }
}
