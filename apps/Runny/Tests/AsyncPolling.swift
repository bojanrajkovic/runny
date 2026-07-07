import Foundation

/// Shared by tests that assert on state changed by a fire-and-forget async
/// Task (DaemonStore.run, LogStreamModel.start, CycleHistoryModel.refresh —
/// none of these are themselves awaitable without changing every production
/// caller's signature). @MainActor to match the callers: all of them read
/// MainActor-isolated state (DaemonStore, LogStreamModel, CycleHistoryModel
/// are all @MainActor) from inside `condition`.
@MainActor
func poll(timeout: TimeInterval = 2, _ condition: () -> Bool) async -> Bool {
    let deadline = Date().addingTimeInterval(timeout)
    while Date() < deadline {
        if condition() { return true }
        try? await Task.sleep(for: .milliseconds(10))
    }
    return condition()
}
