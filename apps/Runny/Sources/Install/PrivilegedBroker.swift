import AppKit
import Foundation

/// The single place the app raises a `do shell script "…" with administrator
/// privileges` prompt and recovers its result. Both privileged installers — the
/// CLI symlink (`CLIInstallModel`) and the system daemon (`SystemDaemonInstaller`)
/// — own a broker and drive it identically: build a shell one-liner, `run` it
/// under one admin prompt, then confirm the effect from disk/socket (never the
/// exit code). Centralizing it keeps ONE audited shell+AppleScript escaping path
/// and ONE process/cancel runner, so a quoting or prompt-handling fix lands for
/// every privileged installer at once instead of in one and not the other.
///
/// What stays with the OWNING model: its generation/cancel state machine and the
/// DOMAIN interpretation of a nonzero exit (the CLI reads exit 3 as a foreign
/// file; the daemon installer reads any nonzero as a failure). The broker owns
/// only the live process handle, so `cancel()` can terminate a standing prompt.
@MainActor
final class PrivilegedBroker {
    /// The outcome of launching osascript: the prompt ran to completion (carrying
    /// its raw exit code + stderr for the caller to interpret), or it never
    /// launched. A user-dismissed prompt is NOT a separate case — it is a completed
    /// run with AppleScript error -128, recovered by `isUserCancelled`.
    enum RunResult: Equatable {
        case completed(exitCode: Int32, stderr: String)
        case launchFailed(String)
    }

    /// The live privileged process, held so `cancel()` can terminate a standing
    /// admin prompt. nil when nothing is in flight.
    private var running: Process?

    /// Raise the admin prompt for `script` and wait. The app is `LSUIElement`/
    /// accessory, so it is activated first to give the system prompt focus. The
    /// process is published and launched with NO suspension in between, so a
    /// concurrent `cancel()` can only reach a LIVE process — never orphan an
    /// unlaunched one. The caller maps the raw exit + stderr to its own outcome and
    /// confirms the real effect from disk/socket.
    func run(_ script: String) async -> RunResult {
        // Bring the app forward so the system admin prompt has focus. Do NOT flip
        // and restore the activation policy here: a blind restore would strand the
        // app `.regular` if its window closed during the prompt (the `LSUIElement`
        // bug `ActivationCoordinator`'s remaining-window check exists to avoid).
        NSApp.activate(ignoringOtherApps: true)

        let proc = Process()
        proc.executableURL = URL(fileURLWithPath: "/usr/bin/osascript")
        proc.arguments = ["-e", script]
        let errPipe = Pipe()
        proc.standardError = errPipe
        proc.standardOutput = Pipe()
        // Publish `running` and launch with no suspension before `proc.run()`, so a
        // Cancel can only land once the process is live (where `running?.terminate()`
        // reaches it) — never on an unlaunched process that would orphan a prompt.
        running = proc
        do {
            try proc.run()
        } catch {
            if running === proc { running = nil }
            return .launchFailed("could not launch osascript: \(error.localizedDescription)")
        }
        // Only the blocking wait goes to a background queue.
        let (status, stderr): (Int32, String) = await withCheckedContinuation { cont in
            DispatchQueue.global().async {
                proc.waitUntilExit()
                let data = errPipe.fileHandleForReading.readDataToEndOfFile()
                cont.resume(returning: (proc.terminationStatus, String(data: data, encoding: .utf8) ?? ""))
            }
        }
        if running === proc { running = nil }
        return .completed(exitCode: status, stderr: stderr)
    }

    /// Terminate a standing admin prompt, if one is up. The owning model bumps its
    /// generation and sets its own loud `.cancelled` state; this only kills the
    /// process so the prompt does not outlive the cancel.
    func cancel() {
        running?.terminate()
        running = nil
    }
}

// MARK: - Pure helpers (nonisolated static, unit-tested without raising a prompt)

extension PrivilegedBroker {
    /// True when osascript reports the user dismissed the auth prompt: AppleScript
    /// error -128, surfaced as the trailing parenthesized code or the localized
    /// "User canceled" string. Only meaningful for a nonzero exit.
    nonisolated static func isUserCancelled(exitCode: Int32, stderr: String) -> Bool {
        exitCode != 0
            && (trailingParenCode(stderr) == -128 || stderr.localizedCaseInsensitiveContains("User canceled"))
    }

    /// The final parenthesized integer in an osascript error string (e.g. "… (3)"
    /// → 3, "… (-128)" → -128), or nil if the message doesn't end in one. `do shell
    /// script` renders the inner shell exit (or AppleScript's own error) as the
    /// FINAL parenthesized number, so matching the trailing token — not a bare
    /// substring — keeps a path or message containing "(3)" earlier from
    /// misclassifying a later failure.
    nonisolated static func trailingParenCode(_ s: String) -> Int? {
        let t = s.trimmingCharacters(in: .whitespacesAndNewlines)
        guard t.hasSuffix(")"), let open = t.lastIndex(of: "(") else { return nil }
        return Int(t[t.index(after: open) ..< t.index(before: t.endIndex)])
    }

    /// Wrap a shell command as `do shell script "…" with administrator privileges`,
    /// escaping for the AppleScript string layer (backslash then double-quote).
    nonisolated static func appleScript(doShell sh: String) -> String {
        let escaped = sh
            .replacingOccurrences(of: "\\", with: "\\\\")
            .replacingOccurrences(of: "\"", with: "\\\"")
        return "do shell script \"\(escaped)\" with administrator privileges"
    }

    /// Single-quote a path for `/bin/sh`, escaping embedded single quotes the
    /// standard `'\''` way — so a path with spaces (or a quote) can't break out of
    /// the privileged command.
    nonisolated static func shellSingleQuote(_ s: String) -> String {
        "'" + s.replacingOccurrences(of: "'", with: "'\\''") + "'"
    }

    /// True when a bundle is translocated or at a transient path — a privileged step
    /// that copies from or links into it would break on next launch. The Security
    /// SPI that answers this authoritatively isn't in the Swift import surface, so
    /// this matches the App Translocation mount root Gatekeeper always uses (no false
    /// negatives for the actual hazard) and fails closed for any `/private/var/folders`
    /// transient path.
    nonisolated static func isTranslocated(_ bundlePath: String) -> Bool {
        bundlePath.contains("/AppTranslocation/") || bundlePath.hasPrefix("/private/var/folders/")
    }
}
