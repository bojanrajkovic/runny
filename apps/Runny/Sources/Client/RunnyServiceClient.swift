import RunnyV1

/// A log stream plus the ability to end the underlying RPC — the seam form of
/// GRPCAsyncServerStreamingCall, whose concrete type isn't publicly
/// constructible (same reason watchStatus bridges to AsyncThrowingStream, see
/// RunnyClient.watchStatus's doc comment) and whose split between iteration
/// (`responseStream`) and explicit `.cancel()` (see RunnyClient.streamLogs's
/// doc comment: cancelling the consuming Task alone leaves the server-side
/// RPC and ring subscription open) a bridged AsyncSequence alone would lose.
struct LogStreamHandle: Sendable {
    let lines: AsyncThrowingStream<Runny_V1_LogLine, Error>
    let cancel: @Sendable () -> Void
}

/// The RunnyClient surface DaemonStore, LogStreamModel, and CycleHistoryModel
/// depend on, extracted so tests can substitute a fake without a real
/// gRPC/unix-socket connection — the same seam shape the daemon side uses for
/// its own exec/verdict test seams (configTester, verdictTester). RunnyClient
/// conforms via the extension in RunnyClient.swift; there is exactly one
/// production conformer.
protocol RunnyServiceClient: AnyObject, Sendable {
    /// Distinguishes which client a supervision attempt is bound to (see
    /// DaemonStore.runStream's "stream bound to RunnyClient[id]" log line).
    var id: UInt16 { get }

    func watchStatus() -> AsyncThrowingStream<Runny_V1_GetStatusResponse, Error>
    func streamLogs(slot: String?, daemon: Bool, replay: UInt32) -> LogStreamHandle
    func recycle(slot: String, reason: String, cancelRunningJob: Bool) async throws
    func pause(slot: String, commandID: String) async throws -> String
    func resume(slot: String, commandID: String) async throws
    func reload(reason: String) async throws -> Runny_V1_ReloadResponse
    func why(slot: String, cycles: UInt32) async throws -> [Runny_V1_CycleRecord]
    func doctor() async throws -> [Runny_V1_DoctorCheck]
    func shutdown() async
}
