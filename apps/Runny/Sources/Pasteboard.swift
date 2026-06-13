import AppKit

/// The general pasteboard copy idiom, in one place — used by the Info-card
/// copy buttons and the artifact "Copy Path" menu.
enum Pasteboard {
    static func copy(_ string: String) {
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(string, forType: .string)
    }
}
