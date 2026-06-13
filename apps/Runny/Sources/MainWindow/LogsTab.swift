import AppKit
import RunnyV1
import SwiftUI

/// Follow-tail log view. NSTextView-backed: SwiftUI text selection is
/// per-Text only, and copying a multi-line excerpt is the whole point of a
/// CI log pane — AppKit gives selection, find, and cheap appends for free.
struct LogsTab: View {
    let slotName: String?
    var daemon = false

    @Environment(DaemonStore.self) private var store
    @State private var model: LogStreamModel?
    /// False during the initial replay window so switching streams doesn't
    /// flash "No logs" before the bounded replay has had a chance to arrive.
    @State private var settled = false

    var body: some View {
        Group {
            if let model {
                if !model.lines.isEmpty {
                    LogTextView(lines: model.lines, daemonMode: daemon)
                } else if settled {
                    ContentUnavailableView(
                        "No logs", systemImage: "text.alignleft",
                        description: Text("Lines appear here as the runner and daemon emit them.")
                    )
                } else {
                    ProgressView()
                }
            } else {
                ProgressView()
            }
        }
        .onAppear {
            let m = LogStreamModel(slot: slotName, daemon: daemon)
            m.start(store: store)
            model = m
            Task {
                try? await Task.sleep(for: .seconds(1.2))
                settled = true
            }
        }
        .onDisappear {
            // Streams die with their view; nothing follows invisibly.
            model?.stop()
            model = nil
        }
    }
}

struct LogTextView: NSViewRepresentable {
    var lines: [LogStreamModel.Line]
    var daemonMode: Bool

    func makeCoordinator() -> Coordinator { Coordinator() }

    func makeNSView(context: Context) -> NSScrollView {
        let scrollView = NSTextView.scrollableTextView()
        let textView = scrollView.documentView as! NSTextView
        textView.isEditable = false
        textView.usesFindBar = true
        textView.autoresizingMask = [.width]
        textView.textContainerInset = NSSize(width: 6, height: 6)
        textView.backgroundColor = .textBackgroundColor
        context.coordinator.textView = textView
        return scrollView
    }

    func updateNSView(_ scrollView: NSScrollView, context: Context) {
        context.coordinator.render(lines: lines, daemonMode: daemonMode, in: scrollView)
    }

    @MainActor
    final class Coordinator {
        weak var textView: NSTextView?
        private var renderedThroughID = -1
        /// (line id, rendered character count), oldest first — lets the text
        /// view shed its prefix in lockstep with the model's ring. Append-only
        /// storage would otherwise grow unboundedly on a long-lived follow
        /// while the model dutifully trims to 5000 lines.
        private var renderedLengths: [(id: Int, length: Int)] = []

        func render(lines: [LogStreamModel.Line], daemonMode: Bool, in scrollView: NSScrollView) {
            guard let textView, let storage = textView.textStorage else { return }

            let wasAtBottom = isPinnedToBottom(scrollView)

            // Trim the prefix the model's ring has already dropped.
            if let firstID = lines.first?.id {
                var dropChars = 0
                var dropCount = 0
                for entry in renderedLengths where entry.id < firstID {
                    dropChars += entry.length
                    dropCount += 1
                }
                if dropCount > 0 {
                    storage.deleteCharacters(in: NSRange(location: 0, length: dropChars))
                    renderedLengths.removeFirst(dropCount)
                }
            } else if !renderedLengths.isEmpty {
                storage.setAttributedString(NSAttributedString())
                renderedLengths.removeAll()
                renderedThroughID = -1
            }

            let fresh = lines.filter { $0.id > renderedThroughID }
            guard !fresh.isEmpty else { return }

            let batch = NSMutableAttributedString()
            for line in fresh {
                let formatted = Self.format(line, daemonMode: daemonMode)
                batch.append(formatted)
                renderedLengths.append((id: line.id, length: formatted.length))
                renderedThroughID = line.id
            }
            storage.append(batch)
            if wasAtBottom {
                textView.scrollToEndOfDocument(nil)
            }
        }

        /// Follow only while the user is at the tail; scrolling up pauses.
        private func isPinnedToBottom(_ scrollView: NSScrollView) -> Bool {
            let visible = scrollView.contentView.bounds
            let docHeight = scrollView.documentView?.frame.height ?? 0
            return visible.maxY >= docHeight - 30
        }

        private static let time: DateFormatter = {
            let formatter = DateFormatter()
            formatter.dateFormat = "HH:mm:ss"
            return formatter
        }()

        private static let bodyFont = NSFont.monospacedSystemFont(
            ofSize: NSFont.smallSystemFontSize, weight: .regular
        )

        /// Runner mode mirrors runnyctl: time, slot, verbatim message.
        /// Daemon mode: time, level, message, sorted k=v attrs.
        static func format(_ line: LogStreamModel.Line, daemonMode: Bool) -> NSAttributedString {
            let out = NSMutableAttributedString()
            func span(_ text: String, _ color: NSColor) {
                out.append(
                    NSAttributedString(
                        string: text,
                        attributes: [.font: bodyFont, .foregroundColor: color]
                    ))
            }
            if line.isMarker {
                span(line.message + "\n", .tertiaryLabelColor)
                return out
            }
            span(time.string(from: line.time) + " ", .secondaryLabelColor)
            if daemonMode {
                span(line.level + " ", levelColor(line.level))
                span(line.message, .labelColor)
                for key in line.attrs.keys.sorted() {
                    span(" \(key)=\(line.attrs[key] ?? "")", .secondaryLabelColor)
                }
            } else {
                if let slot = line.attrs["slot"] {
                    span(slot + " │ ", .secondaryLabelColor)
                }
                span(line.message, .labelColor)
            }
            span("\n", .labelColor)
            return out
        }

        private static func levelColor(_ level: String) -> NSColor {
            switch level.uppercased() {
            case "ERROR": .systemRed
            case "WARN", "WARNING": .systemOrange
            case "DEBUG": .tertiaryLabelColor
            default: .secondaryLabelColor
            }
        }
    }
}
