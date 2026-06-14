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
        /// Random per-command identity (a UUID string). For pause/resume the
        /// daemon records this in the slot's `recentAppliedCommandIds` when
        /// the command actually applies, so confirmation matches the specific
        /// command rather than a coincidentally-matching paused state. `pending`
        /// is keyed by slot, so a failing command clears its entry only when
        /// it's still the current one — otherwise a fast second command on the
        /// same slot loses its tracking.
        let id: String
        let kind: Kind
        let requestedAt: Date
        /// Cycle at request time; a recycle is confirmed when it changes.
        let cycleID: String

        /// One wording for every surface that renders an in-flight command.
        var displayText: String { "\(kind.rawValue) requested…" }
    }

    /// A small bounded FIFO of command ids a snapshot has just confirmed. The
    /// RPC Task's `catch` consults it so a late transport error — a deadline
    /// that fired *after* the daemon already applied the command and a snapshot
    /// already confirmed it — doesn't raise a contradictory "timed out" banner
    /// on a command the slot visibly reflects. Bounded because only a brief
    /// confirm→catch handoff is ever needed (any such error arrives within the
    /// command's RPC deadline); a value type so it's unit-testable in isolation.
    struct RecentlyConfirmed: Equatable {
        private var ids: [String] = []
        let cap: Int
        init(cap: Int = 16) { self.cap = cap }

        mutating func note(_ id: String) {
            ids.append(id)
            if ids.count > cap { ids.removeFirst(ids.count - cap) }
        }

        /// True exactly once per noted id: a confirmation is consumed by the
        /// single error that races it, never suppressing a later real failure.
        mutating func consume(_ id: String) -> Bool {
            guard let i = ids.firstIndex(of: id) else { return false }
            ids.remove(at: i)
            return true
        }
    }

    private(set) var connection: ConnectionState = .connecting
    private(set) var slots: [Runny_V1_SlotStatus] = []
    private(set) var daemonVersion = ""
    private(set) var daemonStarted: Date?
    private(set) var lastUpdate: Date?
    /// Non-empty while the daemon is draining toward a restart (wedge or
    /// config reload): the reason every slot is converging to paused/wedged.
    private(set) var draining = ""
    /// The daemon's wire-protocol version (0 from a daemon that predates the
    /// command-ack contract). Gates pause/resume confirmation: an old daemon
    /// never echoes a command id, so the app can't distinguish applied from
    /// stuck — it reports the command sent-but-unconfirmable rather than
    /// risk a false confirm or a guaranteed false timeout.
    private(set) var protocolVersion: UInt32 = 0

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
    /// Ids a snapshot confirmed in the last confirm→catch window, so a late RPC
    /// error doesn't contradict a confirmation the operator can already see.
    private var recentlyConfirmed = RecentlyConfirmed()

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
    /// The lowest daemon protocol version that echoes command ids, and so the
    /// floor at which pause/resume are confirmable. Kept in lockstep with the
    /// daemon's `WireProtocolVersion`.
    static let ackProtocolVersion: UInt32 = 1

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
        protocolVersion = 0
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
        protocolVersion = snapshot.protocolVersion
        lastUpdate = Date()
        confirmPending()
    }

    // MARK: - Commands (requested vs confirmed)

    /// Is `command` confirmed by `slot`'s current snapshot? Pure and static so
    /// the confirmation contract is unit-testable without a live stream.
    ///
    /// Pause/resume confirm on the command's id being present in the daemon's
    /// `recentAppliedCommandIds` history, with the paused-direction as a sanity
    /// belt: a random id can't collide across a daemon restart, membership
    /// survives concurrent clients clobbering each other (a scalar wouldn't),
    /// and the direction check rejects a stale snapshot that still carries a
    /// prior command's id. Recycle has no echoed id — the daemon doesn't carry
    /// one on the undo/internal re-issue path — so it confirms on a cycle
    /// change, the same observable it always used.
    nonisolated static func isConfirmed(_ command: PendingCommand, by slot: Runny_V1_SlotStatus?) -> Bool {
        switch command.kind {
        case .pause:
            (slot?.recentAppliedCommandIds.contains(command.id) ?? false) && (slot?.paused ?? false)
        case .resume:
            (slot?.recentAppliedCommandIds.contains(command.id) ?? false) && !(slot?.paused ?? true)
        case .recycle:
            slot.map { $0.cycleID != command.cycleID } ?? false
        }
    }

    private func confirmPending() {
        let now = Date()
        for (slotName, command) in pending {
            let slot = slots.first(where: { $0.slot == slotName })
            if Self.isConfirmed(command, by: slot) {
                // Record the id so a straggling RPC error for this exact command
                // (a deadline that fired after the daemon applied it) is
                // swallowed by run()'s catch rather than raising a banner that
                // contradicts the confirmation the operator can already see.
                recentlyConfirmed.note(command.id)
                // Drop the pending. Deliberately DON'T clear commandError here —
                // it's a single shared scalar, and a background snapshot
                // confirming one slot's command must never wipe a genuine
                // not-confirmed/failure banner belonging to a different slot's
                // command (that would be a silent failure, the one thing this
                // surface exists to prevent).
                pending.removeValue(forKey: slotName)
            } else if now.timeIntervalSince(command.requestedAt) > Self.confirmationBound {
                // Expiry happens here only — even for slots the daemon no
                // longer reports — so the entry can't outlive its meaning.
                pending.removeValue(forKey: slotName)
                // A slot that's simply gone from snapshots (config reload, drain)
                // is a benign disappearance, not a command that failed.
                if slot != nil {
                    commandError =
                        "\(command.kind.rawValue) of \(slotName) not confirmed after \(Int(Self.confirmationBound))s — the daemon accepted it but the slot hasn't reflected it"
                }
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

    /// Operations receive the command's random id (to forward on the wire) and
    /// return an optional advisory note (e.g. pause-during-drain); a non-empty
    /// note surfaces as an info banner, distinct from a failure.
    private func run(_ kind: PendingCommand.Kind, slot: Runny_V1_SlotStatus,
                     _ operation: @escaping (RunnyClient, String) async throws -> String?)
    {
        guard let client else {
            commandError = "daemon unreachable — \(kind.rawValue) not sent"
            return
        }
        // Sweep confirmed/expired pendings to ground truth before the guard
        // reads them. pendingCommand(for:) treats an entry as absent the instant
        // it passes the 10s bound, but confirmPending only *removes* it half a
        // second later (or on the next snapshot); a retry in that window would
        // see "no pending", install a fresh entry over the stale one, and lose
        // the original's not-confirmed watchdog. Sweeping first closes the gap,
        // then the guard reads the raw map rather than the time-windowed view.
        confirmPending()
        // One identified pause/resume in flight per slot at a time: a second
        // would install a fresh pending under the same slot key and lose the
        // first's tracking. Reject it; the operator retries once the in-flight
        // command resolves. Recycle confirms on a cycle change, not the id
        // history, so it isn't subject to this.
        if kind == .pause || kind == .resume, pending[slot.slot] != nil {
            commandError =
                "\(kind.rawValue) of \(slot.slot) ignored — a command is already pending for it"
            return
        }
        let id = UUID().uuidString
        // Pause/resume are confirmable only against a daemon that advertises the
        // ack protocol; an older daemon never echoes the id, so installing a
        // pending would guarantee a false 10s not-confirmed timeout. Recycle is
        // confirmed by cycle change and needs no protocol support.
        let confirmable = kind == .recycle || protocolVersion >= Self.ackProtocolVersion
        if confirmable {
            pending[slot.slot] = PendingCommand(
                id: id, kind: kind, requestedAt: Date(), cycleID: slot.cycleID
            )
        }
        Task { @MainActor in
            do {
                let note = try await operation(client, id)
                // A drain warning and the unconfirmable-daemon hint can both
                // apply at once (an old daemon that is also draining): combine
                // them so the upgrade hint isn't hidden behind the drain note.
                var parts: [String] = []
                if let note, !note.isEmpty { parts.append(note) }
                if !confirmable {
                    // Sent, but this daemon can't echo the id — be honest that
                    // the app can't verify it took effect rather than imply it did.
                    parts.append(
                        "\(kind.rawValue) of \(slot.slot) sent — this runnyd predates command confirmation; upgrade runnyd to verify it took effect"
                    )
                }
                if !parts.isEmpty { commandNote = parts.joined(separator: " — ") }
            } catch {
                if error.isDefinitiveRejection {
                    // The daemon proved the command did not apply (full buffer,
                    // no such slot, refused mid-drain). Clear our pending — only
                    // if it's still ours — and surface the error. Never let a
                    // racing confirmation swallow this: a rejected command was
                    // never confirmed, and hiding it would be the silent failure
                    // this surface exists to prevent.
                    if pending[slot.slot]?.id == id { pending.removeValue(forKey: slot.slot) }
                } else if recentlyConfirmed.consume(id) {
                    // Ambiguous error, but a snapshot already confirmed this exact
                    // command — its deadline fired after the daemon applied it and
                    // a snapshot reflected it. Swallow the straggler; surfacing a
                    // failure over a confirmation the operator can see is a lie in
                    // the other direction. Consume-once, so a later real failure on
                    // a different command still surfaces.
                    return
                }
                // Otherwise an ambiguous error keeps the pending: the ack may
                // still arrive, and the 10s confirmation watchdog is the honest
                // backstop. Surface the error either way.
                commandError = Self.describe(error, kind: kind, slot: slot.slot)
            }
        }
        // Drive the confirmation timeout off a timer, not just snapshots: if
        // WatchStatus drops or wedges after the command, confirmPending() would
        // otherwise never run again and the 10s not-confirmed promise would
        // fail silently exactly when supervision is unhealthy. The check is
        // idempotent — a snapshot that already confirmed/expired it is a no-op.
        if confirmable {
            Task { @MainActor in
                try? await Task.sleep(for: .seconds(Self.confirmationBound + 0.5))
                confirmPending()
            }
        }
    }

    func pauseSlot(_ slot: Runny_V1_SlotStatus) {
        run(.pause, slot: slot) { try await $0.pause(slot: slot.slot, commandID: $1) }
    }

    func resumeSlot(_ slot: Runny_V1_SlotStatus) {
        run(.resume, slot: slot) { try await $0.resume(slot: slot.slot, commandID: $1); return nil }
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

    /// The confirmed path. Re-read current state, then guard the cycle: if it
    /// advanced while the dialog was open (e.g. a DEBUG hold auto-released and
    /// a new cycle reached JOB), the confirmation described a guest that's
    /// gone — don't retarget this (possibly destructive) action onto an
    /// unrelated new job. Make the operator re-issue against the live state.
    /// When the cycle is unchanged, cancel the job only if it's STILL JOB (in
    /// DEBUG the recycle destroys the held guest regardless of the flag).
    func confirmRecycle(_ slot: Runny_V1_SlotStatus) {
        recycleConfirm = nil
        guard let current = slots.first(where: { $0.slot == slot.slot }) else { return }
        guard current.cycleID == slot.cycleID else {
            commandNote =
                "\(slot.slot) moved to a new cycle while confirming — recycle not sent; re-issue if it's still needed"
            return
        }
        performRecycle(current, cancelRunningJob: current.state == .job)
    }

    private func performRecycle(_ slot: Runny_V1_SlotStatus, cancelRunningJob: Bool) {
        // Recycle confirms on a cycle change, not an echoed id, so the id arg is
        // unused here — the daemon's RecycleRequest carries no command id.
        run(.recycle, slot: slot) { client, _ in
            try await client.recycle(
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
        case .failedPrecondition:
            // A deliberate refusal with an operator-actionable reason (e.g. a
            // resume while the daemon is draining). Surface the server's message
            // verbatim rather than a generic failure string.
            error.grpcMessage.map { "\(kind.rawValue) of \(slot) rejected — \($0)" }
                ?? "\(kind.rawValue) of \(slot) rejected by the daemon"
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
