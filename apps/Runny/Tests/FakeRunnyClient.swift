@testable import Runny

import RunnyV1

/// A RunnyServiceClient test double: canned responses/errors per RPC, plus a
/// record of every call so tests can assert on dispatch without a real
/// socket or gRPC channel. @unchecked Sendable like RunnyClient itself —
/// tests configure it before use and DaemonStore only ever touches it from
/// the MainActor, so there's no real concurrent access to race.
final class FakeRunnyClient: RunnyServiceClient, @unchecked Sendable {
    let id = UInt16.random(in: 0 ... .max)

    /// watchStatus() yields these in order, then ends — throwing
    /// watchStatusError if set, else finishing cleanly.
    var snapshots: [Runny_V1_GetStatusResponse] = []
    var watchStatusError: Error?

    var recycleError: Error?
    var pauseResult: Result<String, Error> = .success("")
    var resumeError: Error?
    var reloadResult: Result<Runny_V1_ReloadResponse, Error> = .success(.init())
    var doctorResult: Result<[Runny_V1_DoctorCheck], Error> = .success([])
    var whyResult: Result<[Runny_V1_CycleRecord], Error> = .success([])

    /// streamLogs() yields these in order, then ends — throwing
    /// streamLogsError if set, else finishing cleanly. Mirrors watchStatus's
    /// canned-then-end shape.
    var logLines: [Runny_V1_LogLine] = []
    var streamLogsError: Error?

    private(set) var recycleCalls: [(slot: String, reason: String, cancelRunningJob: Bool)] = []
    private(set) var pauseCalls: [(slot: String, commandID: String)] = []
    private(set) var resumeCalls: [(slot: String, commandID: String)] = []
    private(set) var reloadCalls: [String] = []
    private(set) var doctorCallCount = 0
    private(set) var shutdownCallCount = 0
    private(set) var whyCalls: [(slot: String, cycles: UInt32)] = []
    private(set) var streamLogsCalls: [(slot: String?, daemon: Bool, replay: UInt32)] = []
    private(set) var streamLogsCancelCount = 0

    func watchStatus() -> AsyncThrowingStream<Runny_V1_GetStatusResponse, Error> {
        AsyncThrowingStream { continuation in
            for snapshot in snapshots {
                continuation.yield(snapshot)
            }
            if let watchStatusError {
                continuation.finish(throwing: watchStatusError)
            } else {
                continuation.finish()
            }
        }
    }

    func recycle(slot: String, reason: String, cancelRunningJob: Bool) async throws {
        recycleCalls.append((slot, reason, cancelRunningJob))
        if let recycleError { throw recycleError }
    }

    func pause(slot: String, commandID: String) async throws -> String {
        pauseCalls.append((slot, commandID))
        return try pauseResult.get()
    }

    func resume(slot: String, commandID: String) async throws {
        resumeCalls.append((slot, commandID))
        if let resumeError { throw resumeError }
    }

    func reload(reason: String) async throws -> Runny_V1_ReloadResponse {
        reloadCalls.append(reason)
        return try reloadResult.get()
    }

    func doctor() async throws -> [Runny_V1_DoctorCheck] {
        doctorCallCount += 1
        return try doctorResult.get()
    }

    func why(slot: String, cycles: UInt32) async throws -> [Runny_V1_CycleRecord] {
        whyCalls.append((slot, cycles))
        return try whyResult.get()
    }

    func streamLogs(slot: String?, daemon: Bool, replay: UInt32) -> LogStreamHandle {
        streamLogsCalls.append((slot, daemon, replay))
        let stream = AsyncThrowingStream<Runny_V1_LogLine, Error> { continuation in
            for line in logLines {
                continuation.yield(line)
            }
            if let streamLogsError {
                continuation.finish(throwing: streamLogsError)
            } else {
                continuation.finish()
            }
        }
        return LogStreamHandle(lines: stream) { [weak self] in self?.streamLogsCancelCount += 1 }
    }

    func shutdown() async {
        shutdownCallCount += 1
    }
}
