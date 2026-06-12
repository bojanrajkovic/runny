import AppKit
import SwiftUI

@main
struct RunnyApp: App {
    @State private var store = DaemonStore()
    @State private var activation = ActivationCoordinator()

    var body: some Scene {
        MenuBarExtra("Runny", systemImage: "play.rectangle.on.rectangle") {
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
            guard let closing = notification.object as? NSWindow,
                  closing.identifier?.rawValue == WindowID.main
            else { return }
            Task { @MainActor in
                // After this window goes away, revert unless another
                // regular window (Settings, a second main) is still up.
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
    @Environment(DaemonStore.self) private var store
    @AppStorage(RunnyHome.overrideDefaultsKey) private var homeOverride = ""

    var body: some View {
        Form {
            Section {
                TextField("Runny home", text: $homeOverride, prompt: Text("~/.runny"))
                    .help("Where runnyd keeps its socket. Matches the daemon's --home / RUNNY_HOME. Leave empty for ~/.runny.")
                    // Restart on commit, not per keystroke — every restart
                    // tears down and redials the whole client stack.
                    .onSubmit { store.restart() }
                LabeledContent("Socket", value: RunnyHome.displaySocketPath)
                Button("Reconnect") { store.restart() }
            } footer: {
                Text("A daemon started with a custom home is invisible to the app unless this matches — Finder-launched apps never see shell environment variables. Press Return (or Reconnect) to apply.")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
        }
        .formStyle(.grouped)
        .frame(width: 480)
    }
}
