import AppKit
import Foundation
import GRPC
import Observation
import RunnyV1

/// Owns the app's single supervised WatchStatus stream and the connection
/// state machine every surface renders from.
///
/// The supervision contract (silent-failure-proofness, ported from the
/// daemon's invariants):
///  - stream establishment is bounded end-to-end: dial + first snapshot
///    within 5s, or the attempt is torn down (a unix connect() can succeed
///    into the kernel backlog while a wedged daemon never accepts);
///  - a staleness watchdog kills the stream if no snapshot arrives for 90s
///    (the server ticks every 30s even with no transitions — three missed
///    ticks means a wedged-but-alive daemon, rendered as `stale`, never as
///    a healthy green dot);
///  - reconnect backoff 1s → 30s with jitter, reset on machine wake and on
///    socket-file appearance, so the app is never blind a full backoff after
///    the daemon returns;
///  - command RPC success only means "requested": Pause/Resume/Recycle are
///    confirmed from subsequent snapshots, with a 10s not-confirmed surface.
@MainActor
@Observable
final class DaemonStore {
    enum ConnectionState: Equatable {
        case connecting
        case connected
        /// Stream just dropped. Every daemon restart cuts streams within 5s,
        /// so this is routine — rendered amber, not gray, until retries fail.
        case reconnecting
        /// Stream open but snapshots stopped: wedged-but-alive daemon.
        case stale(since: Date)
        case unreachable(reason: String)
    }

    struct PendingCommand: Equatable {
        enum Kind: String { case pause, resume, recycle }
        let kind: Kind
        let requestedAt: Date
        /// Cycle at request time; a recycle is confirmed when it changes.
        let cycleID: String

        /// One wording for every surface that renders an in-flight command.
        var displayText: String { "\(kind.rawValue) requested…" }
    }

    private(set) var connection: ConnectionState = .connecting
    private(set) var slots: [Runny_V1_SlotStatus] = []
    private(set) var daemonVersion = ""
    private(set) var daemonStarted: Date?
    private(set) var lastUpdate: Date?
    /// Non-empty while the daemon is draining toward a restart (wedge or
    /// config reload): the reason every slot is converging to paused/wedged.
    private(set) var draining = ""

    private(set) var doctorChecks: [Runny_V1_DoctorCheck]?
    private(set) var doctorRanAt: Date?
    private(set) var doctorRunning = false

    /// Set when a command fails or goes unconfirmed; views alert and clear.
    var commandError: String?
    /// Advisory note from a command (e.g. pause during a drain is in-memory);
    /// not a failure — surfaced as info and cleared by the view.
    var commandNote: String?
    /// A recycle awaiting operator consent (it would cancel a running job or
    /// destroy a debug hold — the CLI's `-force` cases). The hosting view
    /// presents a confirmation; nil otherwise.
    var recycleConfirm: Runny_V1_SlotStatus?

    private(set) var pending: [String: PendingCommand] = [:]

    /// The client of the current healthy stream; log/timeline views borrow
    /// it. nil while unreachable — actions fail fast instead of hanging.
    private(set) var client: RunnyClient?

    private var supervisor: Task<Void, Never>?
    private var sleepTask: Task<Void, Never>?
    private var retryNow = false
    private var attemptLastMessage: Date?
    private var failedAttemptsSinceConnected = 0
    private var wakeObserver: NSObjectProtocol?
    private var socketWatch: DispatchSourceFileSystemObject?

    static let establishmentBound: TimeInterval = 5
    static let stalenessBound: TimeInterval = 90
    static let confirmationBound: TimeInterval = 10

    func start() {
        guard supervisor == nil else { return }
        // App-lifetime observer: registered once, survives restart() — a
        // re-register per restart leaks a block in NSWorkspace's center.
        if wakeObserver == nil {
            wakeObserver = NSWorkspace.shared.notificationCenter.addObserver(
                forName: NSWorkspace.didWakeNotification, object: nil, queue: .main
            ) { [weak self] _ in
                Task { @MainActor in self?.requestRetryNow() }
            }
        }
        watchHomeDirectory()
        supervisor = Task { await superviseForever() }
    }

    /// Restart from scratch (Settings changed the runny home).
    func restart() {
        supervisor?.cancel()
        supervisor = nil
        socketWatch?.cancel()
        socketWatch = nil
        connection = .connecting
        slots = []
        client = nil
        start()
    }

    private func requestRetryNow() {
        retryNow = true
        sleepTask?.cancel()
    }

    /// Watches the home directory so socket-file appearance retries
    /// immediately instead of waiting out a 30s backoff.
    private func watchHomeDirectory() {
        let fd = open(RunnyHome.directory.path, O_EVTONLY)
        guard fd >= 0 else { return }
        let source = DispatchSource.makeFileSystemObjectSource(
            fileDescriptor: fd, eventMask: .write, queue: .main
        )
        source.setEventHandler { [weak self] in
            if RunnyHome.socketExists {
                Task { @MainActor in self?.requestRetryNow() }
            }
        }
        source.setCancelHandler { close(fd) }
        source.resume()
        socketWatch = source
    }

    // MARK: - Supervision

    private enum StreamOutcome {
        case neverEstablished
        case dropped
        case wentStale
    }

    private func superviseForever() async {
        var backoff: TimeInterval = 1
        while !Task.isCancelled {
            let attemptClient = RunnyClient(socketPath: RunnyHome.socketPath)
            let outcome = await runStream(attemptClient)
            // Identity check: a cancelled supervisor unwinding late must not
            // null out a successor's healthy client (restart() races this).
            if client === attemptClient { client = nil }
            await attemptClient.shutdown()
            if Task.isCancelled { return }

            switch outcome {
            case .dropped:
                backoff = 1
                failedAttemptsSinceConnected = 0
                connection = .reconnecting
            case .wentStale:
                backoff = 1
                failedAttemptsSinceConnected = 0
                connection = .stale(since: lastUpdate ?? Date())
            case .neverEstablished:
                failedAttemptsSinceConnected += 1
                backoff = min(backoff * 2, 30)
                // Two quick failures after a drop is a restart taking a
                // moment; three means nobody is home — say so, with the path.
                if failedAttemptsSinceConnected >= 3 {
                    connection = .unreachable(reason: Self.diagnose())
                }
            }
            await sleepInterruptibly(backoff * Double.random(in: 0.8 ... 1.2))
            if retryNow {
                retryNow = false
                backoff = 1
            }
        }
    }

    /// One cancellable sleep — wake/socket-appearance cancel it for an
    /// instant retry. No polling: zero wakeups while idle.
    private func sleepInterruptibly(_ seconds: TimeInterval) async {
        guard !retryNow else { return }
        let sleeper = Task<Void, Never> { _ = try? await Task.sleep(for: .seconds(seconds)) }
        sleepTask = sleeper
        await sleeper.value
        sleepTask = nil
    }

    private func runStream(_ attemptClient: RunnyClient) async -> StreamOutcome {
        attemptLastMessage = nil
        let attemptStarted = Date()
        let stream = attemptClient.watchStatus()
        var sawSnapshot = false

        struct StaleError: Error {}

        do {
            try await withThrowingTaskGroup(of: Void.self) { group in
                group.addTask { @MainActor [weak self] in
                    for try await snapshot in stream {
                        guard let self else { return }
                        attemptLastMessage = Date()
                        client = attemptClient
                        apply(snapshot)
                    }
                }
                group.addTask { @MainActor [weak self] in
                    while true {
                        try await Task.sleep(for: .seconds(1))
                        guard let self else { return }
                        if let last = attemptLastMessage {
                            if Date().timeIntervalSince(last) > Self.stalenessBound {
                                throw StaleError()
                            }
                        } else if Date().timeIntervalSince(attemptStarted)
                            > Self.establishmentBound
                        {
                            throw StaleError()
                        }
                    }
                }
                // First child to return or throw decides the attempt.
                try await group.next()
                group.cancelAll()
            }
            sawSnapshot = attemptLastMessage != nil
            return sawSnapshot ? .dropped : .neverEstablished
        } catch is StaleError {
            return attemptLastMessage == nil ? .neverEstablished : .wentStale
        } catch {
            return attemptLastMessage == nil ? .neverEstablished : .dropped
        }
    }

    private static func diagnose() -> String {
        if RunnyHome.socketExists {
            return "socket at \(RunnyHome.displaySocketPath) isn't answering — daemon hung or starting?"
        }
        return "no socket at \(RunnyHome.displaySocketPath) — is runnyd running, or using a different home?"
    }

    private func apply(_ snapshot: Runny_V1_GetStatusResponse) {
        connection = .connected
        slots = snapshot.slots.sorted { $0.slot < $1.slot }
        daemonVersion = snapshot.version
        daemonStarted = snapshot.hasDaemonStarted ? snapshot.daemonStarted.dateValue : nil
        draining = snapshot.draining
        lastUpdate = Date()
        confirmPending()
    }

    // MARK: - Commands (requested vs confirmed)

    private func confirmPending() {
        let now = Date()
        for (slotName, command) in pending {
            let slot = slots.first(where: { $0.slot == slotName })
            let confirmed: Bool = switch command.kind {
            case .pause: slot?.paused ?? false
            case .resume: !(slot?.paused ?? true)
            case .recycle: slot.map { $0.cycleID != command.cycleID } ?? false
            }
            if confirmed {
                pending.removeValue(forKey: slotName)
            } else if now.timeIntervalSince(command.requestedAt) > Self.confirmationBound {
                // Expiry happens here only — even for slots the daemon no
                // longer reports — so the entry can't outlive its meaning.
                pending.removeValue(forKey: slotName)
                commandError =
                    "\(command.kind.rawValue) of \(slotName) not confirmed after \(Int(Self.confirmationBound))s — the daemon accepted it but the slot hasn't reflected it"
            }
        }
    }

    /// Read-only: views call this from their bodies, and mutating observed
    /// state mid-render is undefined behavior. Entries past the confirmation
    /// bound read as absent; confirmPending owns the actual removal.
    func pendingCommand(for slot: String) -> PendingCommand? {
        guard let command = pending[slot],
              Date().timeIntervalSince(command.requestedAt) <= Self.confirmationBound
        else { return nil }
        return command
    }

    /// Operations return an optional advisory note (e.g. pause-during-drain);
    /// non-empty surfaces as an info banner, distinct from a failure.
    private func run(_ kind: PendingCommand.Kind, slot: Runny_V1_SlotStatus,
                     _ operation: @escaping (RunnyClient) async throws -> String?)
    {
        guard let client else {
            commandError = "daemon unreachable — \(kind.rawValue) not sent"
            return
        }
        pending[slot.slot] = PendingCommand(
            kind: kind, requestedAt: Date(), cycleID: slot.cycleID
        )
        Task { @MainActor in
            do {
                if let note = try await operation(client), !note.isEmpty {
                    commandNote = note
                }
            } catch {
                pending.removeValue(forKey: slot.slot)
                commandError = Self.describe(error, kind: kind, slot: slot.slot)
            }
        }
    }

    func pauseSlot(_ slot: Runny_V1_SlotStatus) {
        run(.pause, slot: slot) { try await $0.pause(slot: slot.slot) }
    }

    func resumeSlot(_ slot: Runny_V1_SlotStatus) {
        run(.resume, slot: slot) { try await $0.resume(slot: slot.slot); return nil }
    }

    /// Recycling needs operator consent exactly when it would cancel a running
    /// job or destroy a debug hold — the states the CLI guards with `-force`.
    func recycleNeedsConsent(_ slot: Runny_V1_SlotStatus) -> Bool {
        slot.state == .job || slot.state == .debug
    }

    /// Entry point for every Recycle control. Safe states recycle at once; the
    /// `-force` cases stage a confirmation the hosting view presents.
    func requestRecycle(_ slot: Runny_V1_SlotStatus) {
        if recycleNeedsConsent(slot) {
            recycleConfirm = slot
        } else {
            performRecycle(slot, cancelRunningJob: false)
        }
    }

    /// The confirmed path: cancel the job only when the slot was in JOB (in
    /// DEBUG the recycle destroys the held guest regardless of the flag).
    func confirmRecycle(_ slot: Runny_V1_SlotStatus) {
        recycleConfirm = nil
        performRecycle(slot, cancelRunningJob: slot.state == .job)
    }

    private func performRecycle(_ slot: Runny_V1_SlotStatus, cancelRunningJob: Bool) {
        run(.recycle, slot: slot) {
            try await $0.recycle(
                slot: slot.slot, reason: "operator request (Runny)",
                cancelRunningJob: cancelRunningJob
            )
            return nil
        }
    }

    private static func describe(
        _ error: Error, kind: PendingCommand.Kind, slot: String
    ) -> String {
        switch error.grpcCode {
        case .unavailable:
            // The slot's command buffer is full — transient, self-draining
            // (the server's Unavailable, matching InjectDebugKey).
            "\(slot) is not accepting commands right now — try again shortly"
        case .notFound:
            "no slot named \(slot)"
        case .deadlineExceeded:
            "\(kind.rawValue) of \(slot) timed out — daemon busy or hung"
        default:
            "\(kind.rawValue) of \(slot) failed: \(error.localizedDescription)"
        }
    }

    func runDoctor() {
        guard let client, !doctorRunning else { return }
        doctorRunning = true
        Task { @MainActor in
            defer { doctorRunning = false }
            do {
                doctorChecks = try await client.doctor()
                doctorRanAt = Date()
            } catch {
                commandError = "doctor failed: \(error.localizedDescription)"
            }
        }
    }
}
