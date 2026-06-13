import Foundation
import GRPC
import Observation
import RunnyV1

/// One StreamLogs subscription, owned by a visible log view and torn down
/// when it disappears — streams never outlive their views.
///
/// Honesty notes baked into the rendering: the server ring is bounded and
/// fan-out drops lines on slow consumers, so the stream is best-effort; and
/// LogLine carries no sequence key, so a reconnect's replay cannot be
/// deduplicated — we mark the seam visibly instead of splicing silently.
/// Failures render their reason as a marker line; a permanent rejection
/// (unknown slot, invalid filter) stops the stream instead of retrying a
/// request that can never succeed.
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

    static let bufferLimit = 5000
    static let replayDepth: UInt32 = 200

    private var task: Task<Void, Never>?
    /// The live StreamLogs call. Cancelling `task` stops *consuming*, but the
    /// shared channel keeps the RPC (and its server ring subscription) open
    /// until this call is explicitly cancelled.
    private var call: GRPCAsyncServerStreamingCall<Runny_V1_StreamLogsRequest, Runny_V1_LogLine>?
    private var nextID = 0
    private var pendingBatch: [Line] = []
    private var flushScheduled = false
    private var lastMarkerText: String?

    let slot: String?
    let daemon: Bool

    init(slot: String?, daemon: Bool) {
        self.slot = slot
        self.daemon = daemon
    }

    func start(store: DaemonStore) {
        guard task == nil else { return }
        task = Task { @MainActor [weak self, weak store] in
            var delay: TimeInterval = 2
            var interrupted = false
            while !Task.isCancelled {
                guard let self, let store else { return }
                guard let client = store.client else {
                    try? await Task.sleep(for: .seconds(1))
                    continue
                }
                do {
                    let call = client.streamLogs(
                        slot: slot, daemon: daemon, replay: Self.replayDepth
                    )
                    self.call = call
                    var receiving = false
                    for try await line in call.responseStream {
                        if !receiving {
                            receiving = true
                            delay = 2
                            if interrupted {
                                interrupted = false
                                // Marked only once data actually flows again.
                                appendMarker("— reconnected; lines may be missing or duplicated —")
                            }
                        }
                        append(self.line(from: line))
                    }
                    if Task.isCancelled { return }
                    // Clean EOF: the daemon closed the stream (shutdown or
                    // wedge-restart). Routine — say so and retry. The stream
                    // established (even if it carried no lines), so reset the
                    // backoff; otherwise repeated empty streams back off to 30s.
                    delay = 2
                    interrupted = true
                    appendMarker("— log stream closed by the daemon; retrying —")
                } catch {
                    if Task.isCancelled { return }
                    let reason = Self.describe(error)
                    switch error.grpcCode {
                    case .notFound, .invalidArgument:
                        // Permanent for this request shape; retrying forever
                        // would be a silent 0.5 req/s error loop.
                        appendMarker("— log stream rejected: \(reason) —")
                        return
                    default:
                        interrupted = true
                        appendMarker("— log stream interrupted: \(reason); retrying —")
                    }
                }
                // This stream is done; the next iteration opens a fresh call.
                call?.cancel()
                call = nil
                try? await Task.sleep(for: .seconds(delay))
                delay = min(delay * 2, 30)
            }
        }
    }

    func stop() {
        task?.cancel()
        // Cancelling the Task only stops consuming; end the RPC explicitly so
        // the server-side stream and ring subscription don't outlive the view.
        call?.cancel()
        call = nil
        task = nil
    }

    private static func describe(_ error: Error) -> String {
        if let status = error as? GRPCStatus {
            return status.message ?? String(describing: status.code)
        }
        return error.localizedDescription
    }

    private func line(from proto: Runny_V1_LogLine) -> Line {
        defer { nextID += 1 }
        return Line(
            id: nextID, time: proto.time.dateValue, level: proto.level,
            message: proto.message, attrs: proto.attrs, isMarker: false
        )
    }

    /// Markers dedupe consecutively: a flapping stream produces one line per
    /// distinct reason, not a ring full of identical pairs.
    private func appendMarker(_ text: String) {
        guard text != lastMarkerText else { return }
        lastMarkerText = text
        defer { nextID += 1 }
        append(Line(id: nextID, time: Date(), level: "", message: text, attrs: [:], isMarker: true))
    }

    /// Appends are coalesced (~150ms) so a bursty guest can't force a
    /// re-render per line; the buffer is a hard ring at `bufferLimit`.
    private func append(_ line: Line) {
        if !line.isMarker { lastMarkerText = nil }
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
