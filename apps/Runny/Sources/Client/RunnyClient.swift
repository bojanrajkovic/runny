import Foundation
import GRPC
import NIOCore
import NIOPosix
import RunnyV1

/// Thin grpc-swift (v1) client over the daemon's unix socket.
///
/// Every unary RPC carries a deadline; the two server streams are bounded by
/// DaemonStore's supervision (establishment bound + staleness watchdog) —
/// nothing here is allowed to hang silently (the project invariant, ported).
final class RunnyClient: @unchecked Sendable {
    private let group: EventLoopGroup
    private let channel: ClientConnection
    let stub: Runny_V1_RunnyServiceAsyncClient
    let socketPath: String

    /// Deadline tiers: snappy for commands, roomier for disk-touching RPCs.
    static let commandTimeout = TimeAmount.seconds(5)
    static let queryTimeout = TimeAmount.seconds(10)

    init(socketPath: String) {
        self.socketPath = socketPath
        group = PlatformSupport.makeEventLoopGroup(loopCount: 1)
        var configuration = ClientConnection.Configuration.default(
            target: .unixDomainSocket(socketPath),
            eventLoopGroup: group
        )
        // The channel's own dial retries stay tight; reconnect pacing is
        // DaemonStore's job, with visible state — not a hidden channel loop.
        configuration.connectionBackoff = ConnectionBackoff(
            initialBackoff: 0.2, maximumBackoff: 2.0
        )
        channel = ClientConnection(configuration: configuration)
        stub = Runny_V1_RunnyServiceAsyncClient(channel: channel)
    }

    private static func options(_ timeout: TimeAmount) -> CallOptions {
        CallOptions(timeLimit: .timeout(timeout))
    }

    func getStatus() async throws -> Runny_V1_GetStatusResponse {
        try await stub.getStatus(.init(), callOptions: Self.options(Self.commandTimeout))
    }

    /// The long-lived status stream. No time limit here by design: the
    /// supervision bounds (first-snapshot deadline, staleness watchdog) live
    /// in DaemonStore, which owns this stream's lifecycle.
    func watchStatus() -> GRPCAsyncResponseStream<Runny_V1_GetStatusResponse> {
        stub.watchStatus(.init())
    }

    func streamLogs(slot: String?, daemon: Bool, replay: UInt32)
        -> GRPCAsyncResponseStream<Runny_V1_LogLine>
    {
        var request = Runny_V1_StreamLogsRequest()
        request.replay = replay
        request.follow = true
        request.daemon = daemon
        if let slot { request.slot = slot }
        return stub.streamLogs(request)
    }

    func recycle(slot: String, reason: String) async throws {
        var request = Runny_V1_RecycleRequest()
        request.slot = slot
        request.reason = reason
        _ = try await stub.recycle(request, callOptions: Self.options(Self.commandTimeout))
    }

    func pause(slot: String) async throws {
        var request = Runny_V1_PauseRequest()
        request.slot = slot
        _ = try await stub.pause(request, callOptions: Self.options(Self.commandTimeout))
    }

    func resume(slot: String) async throws {
        var request = Runny_V1_ResumeRequest()
        request.slot = slot
        _ = try await stub.resume(request, callOptions: Self.options(Self.commandTimeout))
    }

    func why(slot: String, cycles: UInt32) async throws -> [Runny_V1_CycleRecord] {
        var request = Runny_V1_WhyRequest()
        request.slot = slot
        request.cycles = cycles
        let response = try await stub.why(request, callOptions: Self.options(Self.queryTimeout))
        return response.cycles
    }

    func doctor() async throws -> [Runny_V1_DoctorCheck] {
        let response = try await stub.doctor(
            .init(), callOptions: Self.options(Self.queryTimeout)
        )
        return response.checks
    }

    func shutdown() async {
        try? await channel.close().get()
        // Callback form: present across the NIO versions rules_swift pins.
        group.shutdownGracefully { _ in }
    }
}

extension Error {
    /// The gRPC status code, when this error carries one.
    var grpcCode: GRPCStatus.Code? {
        (self as? GRPCStatus)?.code ?? (self as? GRPCStatusTransformable)?.makeGRPCStatus().code
    }
}
