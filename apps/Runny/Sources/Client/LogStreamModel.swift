import Foundation
import Observation
import RunnyV1

/// One StreamLogs subscription, owned by a visible log view and torn down
/// when it disappears — streams never outlive their views.
///
/// Honesty notes baked into the rendering: the server ring is bounded and
/// fan-out drops lines on slow consumers, so the stream is best-effort; and
/// LogLine carries no sequence key, so a reconnect's replay cannot be
/// deduplicated — we mark the seam visibly instead of splicing silently.
@MainActor
@Observable
final class LogStreamModel {
    struct Line: Identifiable {
        let id: Int
        let time: Date
        let level: String
        let message: String
        let attrs: [String: String]
        let isMarker: Bool
    }

    private(set) var lines: [Line] = []
    private(set) var following = false

    static let bufferLimit = 5000
    static let replayDepth: UInt32 = 200

    private var task: Task<Void, Never>?
    private var nextID = 0
    private var pendingBatch: [Line] = []
    private var flushScheduled = false

    let slot: String?
    let daemon: Bool

    init(slot: String?, daemon: Bool) {
        self.slot = slot
        self.daemon = daemon
    }

    func start(store: DaemonStore) {
        guard task == nil else { return }
        following = true
        task = Task { @MainActor [weak self, weak store] in
            var everConnected = false
            while !Task.isCancelled {
                guard let self, let store else { return }
                guard let client = store.client else {
                    try? await Task.sleep(for: .seconds(1))
                    continue
                }
                if everConnected {
                    append(marker("— reconnected; lines may be missing or duplicated —"))
                }
                do {
                    let stream = client.streamLogs(
                        slot: slot, daemon: daemon, replay: Self.replayDepth
                    )
                    for try await line in stream {
                        everConnected = true
                        append(self.line(from: line))
                    }
                } catch {
                    // Drop reason is rendered, not logged-and-lost.
                }
                if Task.isCancelled { return }
                if everConnected {
                    append(marker("— log stream interrupted —"))
                }
                try? await Task.sleep(for: .seconds(2))
            }
        }
    }

    func stop() {
        task?.cancel()
        task = nil
        following = false
    }

    private func line(from proto: Runny_V1_LogLine) -> Line {
        defer { nextID += 1 }
        return Line(
            id: nextID, time: proto.time.dateValue, level: proto.level,
            message: proto.message, attrs: proto.attrs, isMarker: false
        )
    }

    private func marker(_ text: String) -> Line {
        defer { nextID += 1 }
        return Line(id: nextID, time: Date(), level: "", message: text, attrs: [:], isMarker: true)
    }

    /// Appends are coalesced (~150ms) so a bursty guest can't force a
    /// re-render per line; the buffer is a hard ring at `bufferLimit`.
    private func append(_ line: Line) {
        pendingBatch.append(line)
        guard !flushScheduled else { return }
        flushScheduled = true
        Task { @MainActor [weak self] in
            try? await Task.sleep(for: .milliseconds(150))
            self?.flush()
        }
    }

    private func flush() {
        flushScheduled = false
        guard !pendingBatch.isEmpty else { return }
        lines.append(contentsOf: pendingBatch)
        pendingBatch.removeAll(keepingCapacity: true)
        if lines.count > Self.bufferLimit {
            lines.removeFirst(lines.count - Self.bufferLimit)
        }
    }
}
