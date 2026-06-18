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
    /// SHA-256 (hex) of the config the running daemon loaded at cold start
    /// (empty from a daemon predating the field). The reload verdict compares it
    /// against the hash the reload validated to prove the respawn came up on the
    /// right file.
    private(set) var configSHA256 = ""
    /// The running daemon's random per-process boot id (empty below protocol 2).
    /// It is the respawn discriminator: a reload's verdict fires when this flips
    /// to a value other than the one the accepting process echoed — the
    /// persisted instance id can't discriminate, only a per-process id can.
    private(set) var bootID = ""
    /// The daemon-authoritative drain-progress counter. A reload follower resets
    /// its mid-drain stall timer only when this CHANGES, so a wedged daemon's
    /// 30s heartbeats (frozen drain_seq) can't mask a stalled drain.
    private(set) var drainSeq: UInt64 = 0
    /// True while the exit gate is held (the on-disk config no longer parses).
    /// Authoritative — the stall is suppressed on it rather than parsing
    /// `draining`.
    private(set) var exitHeld = false

    /// The daemon's live Local Network (TCC) grant classification from the latest
    /// snapshot, driving the proactive grant card (`localNetworkCard`). UNSPECIFIED
    /// from a daemon predating the field, or when no snapshot has arrived. This is
    /// the daemon-authoritative signal — NOT the button-gated `doctorChecks`, which
    /// is nil until Run Checks and reports ok until a guest boots.
    private(set) var localNetworkGrant: Runny_V1_LocalNetworkGrant = .unspecified

    /// The current version-skew verdict, recomputed from each snapshot, or nil
    /// when the app and daemon match / the daemon's version isn't known yet / the
    /// app is an unstamped dev build / the daemon is merely newer. Surfaces read
    /// `shownSkew`, which gates this on a live connection and on dismissal — never
    /// `skew` directly.
    private(set) var skew: SkewVerdict?
    /// The skew verdict the operator dismissed, if any. Keyed on the full
    /// `Equatable` verdict (not the version string), so a worsening or
    /// different-axis skew on the same version re-surfaces. Set by the dismiss
    /// control.
    var dismissedSkew: SkewVerdict?

    /// The live skew — gated on a healthy connection only. The main-window card
    /// reads this and renders it as an always-on status row, like the draining
    /// line: the card is the authoritative status surface, so it keeps telling the
    /// truth even after the popover's nag is dismissed. The connection gate lives
    /// in the one `gatedSkew` step both this and `shownSkew` call, so no view
    /// re-implements it. Passing `dismissed: nil` means "this surface ignores
    /// dismissal" — deliberately, so the card never hides a standing condition.
    var visibleSkew: SkewVerdict? {
        Self.gatedSkew(skew: skew, connection: connection, dismissed: nil)
    }

    /// `visibleSkew` minus what the operator dismissed — the popover's dismissible
    /// banner reads this, so a dismissal silences the glanceable nag while the card
    /// keeps showing the standing condition.
    var shownSkew: SkewVerdict? {
        Self.gatedSkew(skew: skew, connection: connection, dismissed: dismissedSkew)
    }

    /// What the post-upgrade daemon-update surface should show. The app can update
    /// the daemon (by reloading onto the freshly-bundled binary) only when it
    /// installed the agent AND it is the newer build — a brew/manual daemon would
    /// drain its fleet for a respawn of the same old binary, so it is offered only
    /// the generic skew banner, never this.
    enum DaemonUpdate: Equatable {
        case none
        /// App-installed agent, app newer than the running daemon — offer Update.
        case available
        /// An update reload is draining toward its respawn.
        case inProgress
        /// The reload resolved but the daemon is still the old version — surfaced
        /// loud (named, with the stuck version), never the generic reload note.
        case didNotTake(daemonCore: String)
    }

    /// Set once an Update reload is ACCEPTED (drain started), reset when the update
    /// takes (the daemon stops being older) or on a reconnect. Distinguishes "an
    /// update is available" from "you tried and it didn't take". NOT set merely by
    /// clicking Update — a cancelled confirmation must leave it false.
    private(set) var daemonUpdateAttempted = false

    /// Transient: the pending reload was requested as a daemon UPDATE (vs a plain
    /// config reload), so its acceptance sets `daemonUpdateAttempted`. Consumed by
    /// `performReload`; cleared by a plain `requestReload` so a regular reload after
    /// a cancelled update can't inherit the stale intent.
    private var pendingUpdateIntent = false

    /// Is the app a strictly newer build than the running daemon? The upgrade
    /// direction the symmetric skew verdict does not itself report.
    var appNewerThanDaemon: Bool {
        Self.appNewerThanDaemon(appVersion: Self.appVersion, daemonVersion: daemonVersion)
    }

    /// Same version core, daemon's protocol older than this app expects — the
    /// protocol-only upgrade window.
    var protocolBehind: Bool {
        Self.protocolBehind(
            appVersion: Self.appVersion, daemonVersion: daemonVersion,
            daemonProtocol: protocolVersion, appExpectedProtocol: Self.expectedProtocolVersion
        )
    }

    /// The app is ahead of the running daemon on either axis — a reload would
    /// update it. The condition that gates the update affordance and clears the
    /// attempt flag once an update takes.
    var appAheadOfDaemon: Bool { appNewerThanDaemon || protocolBehind }

    /// Slots with a live in-process guest that uninstalling would abandon —
    /// matching the FSM's own consent rule (`.job` OR `.debug`): a DEBUG-held guest
    /// is a parked VM an operator deliberately kept alive, so booting out the daemon
    /// destroys it just as surely as a running job. The uninstall confirmation names
    /// these slots rather than tearing down silently.
    var liveGuestSlots: [String] {
        slots.filter { $0.state == .job || $0.state == .debug }.map(\.slot)
    }

    /// The update surface, gated on a live connection (a stale verdict from a
    /// dropped daemon must not linger — the Start affordance owns "daemon down").
    /// `agentInstalled` is the app-installed-agent gate the view supplies from
    /// `AgentController` (DaemonStore doesn't observe the lifecycle layer).
    func daemonUpdate(agentInstalled: Bool, agentCanonical: Bool, runningBundleCanonical: Bool) -> DaemonUpdate {
        guard case .connected = connection else { return .none }
        return Self.daemonUpdate(
            agentInstalled: agentInstalled,
            agentCanonical: agentCanonical,
            runningBundleCanonical: runningBundleCanonical,
            appNewer: appNewerThanDaemon,
            protocolBehind: protocolBehind,
            daemonCore: Self.versionCore(daemonVersion) ?? daemonVersion,
            reloadPending: reloadPending,
            attempted: daemonUpdateAttempted
        )
    }

    private(set) var doctorChecks: [Runny_V1_DoctorCheck]?
    private(set) var doctorRanAt: Date?
    private(set) var doctorRunning = false

    /// Set when a command fails or goes unconfirmed; views alert and clear.
    /// `didSet` nils the provenance on every write, so a view clearing the
    /// banner (`= nil`) or a non-command assignment (e.g. a file-not-found
    /// message) correctly leaves no stale id behind; `setCommandError` re-sets
    /// the id immediately after for command-path banners. The provenance lets
    /// `confirmPending` retract *this* command's banner when a later snapshot
    /// confirms it, without wiping a different slot's banner (the scalar is
    /// shared across slots).
    var commandError: String? {
        didSet { commandErrorID = nil }
    }

    /// The id of the command whose error `commandError` currently reflects, or
    /// nil for a banner with no owning command. Read only by `confirmPending`.
    private var commandErrorID: String?

    /// Set the command banner and record which command it belongs to. The
    /// assignment fires `commandError`'s didSet (clearing the id), then we set
    /// the real provenance — so the final state is `(text, id)` regardless of
    /// what the id was before.
    private func setCommandError(_ text: String, id: String?) {
        commandError = text
        commandErrorID = id
    }

    /// Advisory note from a command (e.g. pause during a drain is in-memory);
    /// not a failure — surfaced as info and cleared by the view.
    var commandNote: String?
    /// A recycle awaiting operator consent (it would cancel a running job or
    /// destroy a debug hold — the CLI's `-force` cases). The hosting view
    /// presents a confirmation; nil otherwise.
    var recycleConfirm: Runny_V1_SlotStatus?
    /// True while the operator's reload confirmation dialog is up.
    var reloadConfirm = false
    /// True from the moment a reload RPC is sent until the daemon answers
    /// accepted/refused — disables the Reload control and shows "Validating…".
    /// The preflight is synchronous and can take most of a minute.
    private(set) var reloadInFlight = false

    private(set) var pending: [String: PendingCommand] = [:]
    /// Ids a snapshot confirmed in the last confirm→catch window, so a late RPC
    /// error doesn't contradict a confirmation the operator can already see.
    private var recentlyConfirmed = RecentlyConfirmed()

    /// A reload accepted and awaiting its respawn: the accepting process's boot
    /// id (so a genuinely new process is recognizable — instance id persists
    /// across a respawn, boot id does not), the config hash the reload validated
    /// (so the respawn can be confirmed to have loaded that file), and the
    /// pre-reload start time as the protocol-1 fallback discriminator. nil when
    /// no reload is in flight.
    private var pendingReload: PendingReload?
    /// True while a reload is draining toward its respawn (a pendingReload is
    /// armed) — the window in which a manual Reconnect would tear down the stream
    /// and silently discard the convergence verdict the operator is waiting on.
    /// The daemon-card Reconnect is disabled on this; restart()'s teardown is
    /// otherwise unchanged.
    var reloadPending: Bool { pendingReload != nil }
    /// Whether a slot was running a job in the last old-process snapshot before
    /// the respawn — colors the success verdict (a job may have been
    /// interrupted). Tracked across the drain, read when the respawn resolves.
    private var reloadJobInFlight = false
    /// drain_seq the mid-drain stall is anchored on, and when it last changed.
    /// A frozen drain_seq past `reloadStallBound` (with nothing long-running or
    /// held) is the wedged-but-serving daemon the silence deadline can't catch.
    private var reloadStallSeq: UInt64 = 0
    private var reloadStallSince: Date?
    /// The in-flight reload RPC's task, held so `restart()` can cancel it —
    /// otherwise a late "accepted" answered after a supervisor teardown would arm
    /// a pending reload against the freshly-dialed supervisor and produce a
    /// verdict the app can no longer correctly track. Reconnect is disabled once a
    /// reload is pending, but the RPC-in-flight window *before* the accept is not,
    /// so this cancellation is still load-bearing for that window.
    private var reloadTask: Task<Void, Never>?
    /// Monotonic reload identity. Cancellation is cooperative: a cancelled reload
    /// task's `defer { reloadInFlight = false }` still runs as it unwinds, which
    /// would clear the flag for a reload started after restart() bumped this. The
    /// defer compares its captured generation against the live one and no-ops when
    /// it is stale, so only the current reload can clear its own flag.
    private var reloadGeneration = 0

    struct PendingReload: Equatable {
        let acceptingBootID: String
        let priorStart: Date?
        let wantSHA: String
        /// When the reload was accepted — the floor for the respawn-silence
        /// deadline, so a stream already quiet when Reload was clicked cannot bank
        /// that pre-acceptance silence against the respawn wait.
        let acceptedAt: Date
    }

    /// The fingerprint of a respawn against the config a reload validated. A
    /// `.failure` is config drift — the respawn loaded a different file, the one
    /// outcome the operator must act on; `.warning` is degraded-but-ok (config
    /// unverifiable, or a job may have been interrupted); `.success` is a
    /// confirmed clean reload.
    struct ReloadOutcome: Equatable {
        enum Severity { case success, warning, failure }
        let text: String
        let severity: Severity
    }

    /// A version/protocol skew between this app and the daemon it watches. There
    /// is deliberately no `severity`: skew is ALWAYS a warning by construction (a
    /// `SkewVerdict` exists only to warn — it never disables a control or drops a
    /// connection), so no single-case enum fakes one. The `kind` is the
    /// machine-readable axis — read it, never string-match the `text` (the same
    /// anti-re-parsing discipline the wire contract enforces for
    /// `draining`/`exit_held`). `Equatable` so a dismissal keys on the whole value:
    /// a worsening or different-axis skew on the same version re-surfaces.
    struct SkewVerdict: Equatable {
        enum Kind: Equatable { case versionMismatch, protocolBehind }
        let kind: Kind
        let text: String
    }

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
    /// After a reload drains the fleet, how long the app tolerates SILENCE — no
    /// fresh snapshot — before declaring the respawn failed. Anchored on the
    /// last snapshot (`lastUpdate`), not the reload time: a long healthy drain
    /// keeps serving status every ≤30s, so silence only accrues once the daemon
    /// actually stops reporting (died and hasn't returned). Same budget as
    /// runnyctl's respawn cap.
    static let respawnBound: TimeInterval = 90
    /// How long a reload's drain may make NO progress (a frozen drain_seq) while
    /// the daemon still answers, before it's called wedged. Distinct from
    /// `respawnBound` (silence): this catches the daemon that heartbeats but
    /// stops draining. Generous, since legitimate drain transitions can be
    /// spaced; suppressed while a slot is in JOB/ENSURE_IMAGE or the gate is held.
    static let reloadStallBound: TimeInterval = 90
    /// The lowest daemon protocol version that echoes command ids, and so the
    /// floor at which pause/resume are confirmable. Kept in lockstep with the
    /// daemon's `WireProtocolVersion`.
    static let ackProtocolVersion: UInt32 = 1
    /// The wire-protocol version this app's stubs were built against — the exact
    /// protocol the app expects, set to the literal current `WireProtocolVersion`.
    /// A daemon reporting a lower number predates a capability the app relies on
    /// (the upgrade-window skew the matched `x.y.z` cores hide). Kept in lockstep
    /// with the daemon's `WireProtocolVersion`: bump both together. This is not a
    /// backstop or a cap — the healthy-magnitude sizing rule does not apply.
    static let expectedProtocolVersion: UInt32 = 2
    /// The version a build with no embedded stamp falls back to (the build's
    /// `fallback_build_label`). Both the missing-key coalesce below and the skew
    /// verdict's dev-build guard key on it, so the two halves of "this is an
    /// unstamped build" can't drift apart.
    static let unstampedVersion = "0.0.0"
    /// The app's own stamped version core — `CFBundleShortVersionString`, already
    /// regex-stripped to `x.y.z` by the build (`apple_bundle_version`). A missing
    /// or non-string read coalesces to `unstampedVersion` — the same quiet branch
    /// a dev build takes — so a wrong or missing key fails safe (quiet), never
    /// loud-and-wrong. The one impure read in the skew path; the verdict never
    /// touches the bundle (it takes this as a parameter).
    static let appVersion: String =
        Bundle.main.infoDictionary?["CFBundleShortVersionString"] as? String ?? unstampedVersion

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

    /// Manual re-dial: tear down the supervisor and socket watch, then start()
    /// fresh against the same daemon at the fixed ~/.runny (the daemon-card
    /// Reconnect). That affordance is disabled while `reloadPending`, so this
    /// never runs mid-drain; its full state wipe — including pendingReload and the
    /// reloadGeneration bump — is reachable only when no reload verdict is live.
    func restart() {
        supervisor?.cancel()
        supervisor = nil
        socketWatch?.cancel()
        socketWatch = nil
        connection = .connecting
        slots = []
        protocolVersion = 0
        configSHA256 = ""
        bootID = ""
        drainSeq = 0
        exitHeld = false
        // A reconnect re-establishes the stream from scratch, so a stale skew —
        // and any dismissal of it — must not carry across: the verdict is
        // recomputed against whatever the re-dial actually reaches.
        skew = nil
        dismissedSkew = nil
        daemonUpdateAttempted = false
        pendingReload = nil
        reloadJobInFlight = false
        reloadStallSince = nil
        reloadTask?.cancel()
        reloadTask = nil
        reloadGeneration += 1
        reloadInFlight = false
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
        // On a fresh install the home doesn't exist until the first daemon run,
        // so O_EVTONLY would fail and the socket-appearance fast-retry would
        // never arm — the app would wait out a full reconnect backoff. Create
        // the top-level home (0700, matching the daemon) so the watch arms even
        // on a clean machine.
        RunnyHome.ensureDirectory()
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
            // The connection state just changed; if a reload is waiting on a
            // respawn whose daemon went silent, this is where we give up on it.
            checkReloadRespawnDeadline()
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
        configSHA256 = snapshot.configSha256
        bootID = snapshot.bootID
        drainSeq = snapshot.drainSeq
        exitHeld = snapshot.exitHeld
        localNetworkGrant = snapshot.localNetworkGrant
        lastUpdate = Date()
        confirmPending()
        trackReloadDrain()
        noteRespawnIfReady()
        // Derived from this snapshot's version/protocol against the app's own
        // stamped facts; recompute last so it reflects the values just applied.
        skew = Self.skewVerdict(
            appVersion: Self.appVersion, appExpectedProtocol: Self.expectedProtocolVersion,
            daemonVersion: daemonVersion, daemonProtocol: protocolVersion
        )
        // Once the app is no longer ahead on either axis — the update took, or it
        // was never behind — clear the attempt flag so a future skew shows
        // "available", not a stale "didn't take".
        if !appAheadOfDaemon { daemonUpdateAttempted = false }
    }

    // MARK: - Commands (requested vs confirmed)

    /// Is `command` confirmed by `slot`'s current snapshot? Pure and static so
    /// the confirmation contract is unit-testable without a live stream.
    ///
    /// Pause/resume confirm on the command's id being present in the daemon's
    /// `recentAppliedCommandIds` history — membership alone, no paused-direction
    /// check. The daemon appends the id only when the command actually applies
    /// (inside `setPaused`), so membership already proves application; the random
    /// id can't collide across a daemon restart, and a history (not a scalar)
    /// survives concurrent clients clobbering each other's ack. A direction belt
    /// would be worse than redundant: a fast superseding command (a resume right
    /// after our pause applied) flips `paused` before our next snapshot, so
    /// `&& paused` would reject a pause that *did* run — and the pending would
    /// then time out into a false not-confirmed banner. Recycle has no echoed id
    /// — the daemon doesn't carry one on the undo/internal re-issue path — so it
    /// confirms on a cycle change, the same observable it always used.
    nonisolated static func isConfirmed(_ command: PendingCommand, by slot: Runny_V1_SlotStatus?) -> Bool {
        switch command.kind {
        case .pause, .resume:
            slot?.recentAppliedCommandIds.contains(command.id) ?? false
        case .recycle:
            slot.map { $0.cycleID != command.cycleID } ?? false
        }
    }

    /// Should confirming `confirmedID` retract the current command banner, whose
    /// provenance is `errorID`? Only when the banner belongs to that exact
    /// command. The banner is a single scalar shared across slots, so a
    /// confirmation must never clear a *different* command's failure banner — a
    /// `nil` provenance (a banner with no owning command, e.g. unreachable or a
    /// file-not-found) never matches. Pure and static so the invariant is
    /// testable in isolation, like `isConfirmed`.
    nonisolated static func bannerBelongs(to errorID: String?, confirmedID: String) -> Bool {
        errorID == confirmedID
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
                pending.removeValue(forKey: slotName)
                // Retract this command's own error banner if it has one: an
                // ambiguous error (a transport drop, a deadline) may have set a
                // banner while keeping the pending, and the command then
                // confirmed. Clear it only when the banner belongs to THIS
                // command — the scalar is shared across slots, so without the
                // provenance check a confirmation here could wipe a genuine
                // failure banner for a different slot (a silent failure, the one
                // thing this surface exists to prevent).
                if Self.bannerBelongs(to: commandErrorID, confirmedID: command.id) {
                    commandError = nil
                }
            } else if now.timeIntervalSince(command.requestedAt) > Self.confirmationBound {
                // Expiry happens here only — even for slots the daemon no
                // longer reports — so the entry can't outlive its meaning.
                pending.removeValue(forKey: slotName)
                // A slot that's simply gone from snapshots (config reload, drain)
                // is a benign disappearance, not a command that failed.
                if slot != nil {
                    setCommandError(
                        "\(command.kind.rawValue) of \(slotName) not confirmed after \(Int(Self.confirmationBound))s — the daemon accepted it but the slot hasn't reflected it",
                        id: command.id
                    )
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
        // One command in flight per slot at a time, regardless of kind. pending
        // is keyed by slot, so a second command — including a recycle over a
        // pending pause/resume, or vice versa — would install a fresh entry
        // under the same key and lose the first's not-confirmed watchdog. Reject
        // it; the operator retries once the in-flight command resolves (≤10s).
        if pending[slot.slot] != nil {
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
                // backstop. Surface the error either way, tagged with this
                // command's id so a later confirmation can retract it.
                setCommandError(Self.describe(error, kind: kind, slot: slot.slot), id: id)
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

    // MARK: - Reload (validate → drain → confirm the respawn)

    /// Stage the reload confirmation dialog. Reload restarts the whole daemon
    /// and drains every slot (jobs finish first), so it's gated behind explicit
    /// consent like the `-force` recycle cases.
    func requestReload() {
        pendingUpdateIntent = false
        reloadConfirm = true
    }

    /// Issue a daemon update: identical to a reload (drain jobs, then exit for
    /// launchd to cold-start the freshly-bundled binary), tagged so a non-converged
    /// result surfaces "update didn't take" rather than the generic reload note.
    /// Inherits the reload's drain-stall arm and convergence confirmation wholesale.
    /// The intent is consumed only when the reload is ACCEPTED (in performReload),
    /// so cancelling the confirmation leaves no "didn't take" residue.
    func requestDaemonUpdate() {
        pendingUpdateIntent = true
        reloadConfirm = true
    }

    /// The confirmed path: send the reload. Acceptance arms a pendingReload that
    /// `noteRespawnIfReady` resolves into a verdict once a new daemon (a changed
    /// boot id) answers; refusal surfaces the failed checks at once and leaves the
    /// daemon running — and, when a prior reload is already accepted and draining,
    /// leaves that one's pending tracking intact (only an accepted reload arms it).
    func performReload() {
        reloadConfirm = false
        guard let client else {
            commandError = "daemon unreachable — reload not sent"
            return
        }
        guard !reloadInFlight else { return }
        reloadInFlight = true
        // Consume the update intent synchronously, before the Task suspends, so a
        // concurrent request can't change it mid-flight. Only an ACCEPTED update
        // reload (below) records the attempt.
        let isUpdate = pendingUpdateIntent
        pendingUpdateIntent = false
        // The protocol-1 fallback discriminator. The real discriminator is the
        // accepting process's boot id, carried in the response, so there is no
        // pre-RPC read whose process could differ from the one that accepts.
        let priorStart = daemonStarted
        let gen = reloadGeneration
        reloadTask = Task { @MainActor in
            defer { if gen == reloadGeneration { reloadInFlight = false } }
            do {
                let resp = try await client.reload(reason: "operator request (Runny)")
                guard !Task.isCancelled else { return }
                // Only an accepted reload (re)arms the pending tracking. A refusal
                // must NOT cancel an earlier accepted reload still draining toward
                // its respawn — that one keeps its convergence verdict; the refusal
                // only surfaces the failed checks.
                pendingReload = Self.pendingAfterAttempt(
                    existing: pendingReload,
                    accepted: resp.accepted
                        ? PendingReload(
                            acceptingBootID: resp.acceptingBootID, priorStart: priorStart,
                            wantSHA: resp.configSha256, acceptedAt: Date()
                        )
                        : nil
                )
                guard resp.accepted else {
                    commandError = Self.describeRefusal(resp)
                    return
                }
                // An accepted update reload records the attempt — so a respawn that
                // comes back still older surfaces "update didn't take", while a
                // cancelled or refused one never does.
                if isUpdate { daemonUpdateAttempted = true }
                // Seed from the slots visible at acceptance, so a daemon that
                // dies before its next snapshot still carries a job-in-flight
                // warning into the verdict; later old-process snapshots refine it.
                reloadJobInFlight = Self.anyJobRunning(slots)
                reloadStallSeq = drainSeq
                reloadStallSince = Date()
                var notes = resp.warnings.filter { !$0.ok }.map { "\($0.name): \($0.detail)" }
                if resp.acceptingBootID.isEmpty {
                    notes.append("this daemon predates boot-id reporting — confirming the respawn by start time only; a stalled drain can't be detected either (no drain-progress signal before protocol 2)")
                }
                if !notes.isEmpty {
                    commandNote = "reload accepted — " + notes.joined(separator: "; ")
                }
                // The drain itself shows through the live WatchStatus stream; the
                // respawn is resolved by noteRespawnIfReady on a later snapshot.
            } catch {
                if Task.isCancelled { return }
                // A reload throw is a transport drop, a deadline, or a definitive
                // refusal — never a validation refusal (those come back as
                // accepted == false). A transport drop/deadline is AMBIGUOUS: the
                // daemon may have accepted the reload and begun draining, so the
                // banner must not assert a failure that may not have happened. (We
                // don't arm a pending here: the validated config hash was lost with
                // the response, and a blind pending would later surface a false
                // "isn't converging" if the reload never actually took.) An earlier
                // accepted reload's pending is untouched either way.
                commandError = Self.reloadThrowBanner(error)
            }
        }
    }

    /// Pure: the operator banner for a reload that threw. A definitive rejection
    /// (the daemon refused before acting) is a real failure, surfaced verbatim;
    /// any other throw — a transport drop or a deadline — is AMBIGUOUS, since the
    /// daemon may have accepted the reload and begun draining. The ambiguous banner
    /// says the outcome is unknown and how to confirm it rather than claiming a
    /// failure that may not have happened. Static so the wording is unit-testable;
    /// reuses the same definitive-vs-ambiguous split the command path uses.
    nonisolated static func reloadThrowBanner(_ error: Error) -> String {
        if error.isDefinitiveRejection {
            return "reload failed: " + (error.grpcMessage ?? error.localizedDescription)
        }
        return "the daemon didn't confirm the reload (" + error.localizedDescription
            + "); it may have accepted it and started draining — check `runnyctl status`, "
            + "then re-run reload if it didn't take"
    }

    /// Pure: the pending reload after an attempt resolves. Only an accepted reload
    /// changes it (the `accepted` value); a refusal or transport failure (nil)
    /// returns the EXISTING pending untouched — an earlier accepted reload is still
    /// draining toward its respawn and keeps its convergence verdict rather than
    /// being cancelled by a later attempt. Static so the rule is unit-testable.
    nonisolated static func pendingAfterAttempt(
        existing: PendingReload?, accepted: PendingReload?
    ) -> PendingReload? {
        accepted ?? existing
    }

    /// Is `st`'s identity a genuinely new process versus the reload we baselined?
    /// Prefer boot id (positive, closes the sub-RPC race) when both sides speak
    /// it; otherwise fall back to a changed start time — covering both a pre-2
    /// daemon we baselined and a respawn that downgraded to a pre-2 binary
    /// mid-reload (which echoes no boot id, so a boot-id-only check would pin the
    /// pending reload forever with no verdict).
    private func isReloadSuccessor(_ reload: PendingReload) -> Bool {
        if !reload.acceptingBootID.isEmpty, !bootID.isEmpty {
            return bootID != reload.acceptingBootID
        }
        guard let prior = reload.priorStart, let started = daemonStarted else { return false }
        return started != prior
    }

    /// On each old-process snapshot during a pending reload's drain: remember
    /// whether a job is in flight (colors the verdict) and bound the drain by
    /// PROGRESS. A frozen drain_seq past `reloadStallBound`, with nothing
    /// long-running (JOB/ENSURE_IMAGE are daemon-bounded) or held to explain it,
    /// is a wedged-but-serving daemon — the case `lastUpdate` silence can't catch
    /// because the heartbeat keeps it fresh.
    private func trackReloadDrain() {
        guard let reload = pendingReload, !isReloadSuccessor(reload) else { return }
        reloadJobInFlight = Self.anyJobRunning(slots)
        if drainSeq != reloadStallSeq {
            reloadStallSeq = drainSeq
            reloadStallSince = Date()
            return
        }
        guard let since = reloadStallSince else {
            reloadStallSince = Date()
            return
        }
        if Self.drainStalled(
            protocolVersion: protocolVersion, stalledFor: Date().timeIntervalSince(since),
            bound: Self.reloadStallBound, anySlotActive: Self.anySlotActive(slots), exitHeld: exitHeld
        ) {
            commandError = "reload isn't converging — the daemon stopped making drain "
                + "progress and may be hung; check `runnyctl status`"
            pendingReload = nil
            reloadStallSince = nil
        }
    }

    /// Pure: should a mid-drain reload be declared wedged? Only protocol >= 2
    /// publishes `drain_seq`, the progress signal the stall rests on; a pre-2
    /// daemon pins it at 0, so its drain can't be progress-bounded and must not
    /// trip the stall — which would degrade into a wall-clock cap on a drain that
    /// can validly run as long as any bounded state allows. Also suppressed while
    /// any slot is still working an active state (each is bounded daemon-side by
    /// its own per-state deadline — PROVISION alone is 180s, twice the window) or
    /// the exit gate is held. Static so the gate is unit-testable without a live
    /// daemon; mirrors runnyctl's stall carve-out in `streamDrain`.
    nonisolated static func drainStalled(
        protocolVersion: UInt32, stalledFor: TimeInterval, bound: TimeInterval,
        anySlotActive: Bool, exitHeld: Bool
    ) -> Bool {
        guard protocolVersion >= 2 else { return false }
        return !anySlotActive && !exitHeld && stalledFor > bound
    }

    /// Whether any slot is running a job. The reload's job-in-flight seed (at
    /// acceptance) and its per-snapshot refinement share this, so a job present
    /// when the daemon goes down is caught even if no further snapshot arrives.
    /// Only a running JOB counts — a pull or a debug hold is not an interrupted job.
    nonisolated static func anyJobRunning(_ slots: [Runny_V1_SlotStatus]) -> Bool {
        slots.contains { $0.state == .job }
    }

    /// Whether any slot is still working toward convergence. Mirrors the daemon's
    /// own stable predicate (Wedged || (Paused && BACKOFF)): a slot is quiescent
    /// only when wedged or PAUSED in BACKOFF, so a slot working a cycle state OR
    /// sitting UNPAUSED in BACKOFF (still backing off, up to the backoff cap,
    /// before the drainer's pause lands) counts as active. Each active case is
    /// bounded daemon-side, so a frozen drain_seq while a slot is active is that
    /// bound's business, not a hang. The stall fires only once every slot is
    /// quiescent yet the daemon still hasn't exited. Mirrors runnyctl's
    /// `anySlotActive`.
    nonisolated static func anySlotActive(_ slots: [Runny_V1_SlotStatus]) -> Bool {
        slots.contains { slot in
            guard !slot.wedged, slot.state != .unspecified else { return false }
            return !(slot.state == .backoff && slot.paused)
        }
    }

    /// Once a genuinely new daemon answers while a reload is pending, render the
    /// verdict and retire the pending. Until then a no-op — the old process
    /// answering during the drain resolves nothing.
    private func noteRespawnIfReady() {
        guard let reload = pendingReload, isReloadSuccessor(reload) else { return }
        let outcome = Self.respawnVerdict(
            protocolVersion: protocolVersion,
            gotSHA: configSHA256,
            wantSHA: reload.wantSHA,
            jobInFlight: reloadJobInFlight,
            reDraining: draining
        )
        pendingReload = nil
        reloadStallSince = nil
        switch outcome.severity {
        case .failure:
            commandError = outcome.text
        case .success, .warning:
            commandNote = outcome.text
        }
    }

    /// Bounds the respawn wait from the silence side: if a reload is pending and
    /// no snapshot has arrived for `respawnBound`, the daemon died and never came
    /// back — say so and retire the pending so a much-later unrelated restart
    /// can't surface a stale "reloaded" verdict. The wedged-but-heartbeating case
    /// is `trackReloadDrain`'s job (this anchor stays fresh under a heartbeat).
    private func checkReloadRespawnDeadline() {
        guard let reload = pendingReload else { return }
        guard Self.respawnSilenceExpired(
            acceptedAt: reload.acceptedAt, lastUpdate: lastUpdate,
            now: Date(), bound: Self.respawnBound
        ) else { return }
        if case .unreachable = connection {
            commandError = "reload drained the fleet, but the daemon hasn't "
                + "come back — \(Self.diagnose())"
        } else {
            commandError = "reload isn't converging — the daemon stopped "
                + "reporting while draining and may be hung; check `runnyctl status`"
        }
        pendingReload = nil
        reloadStallSince = nil
    }

    /// Pure: has the respawn-silence deadline passed? Silence is measured from the
    /// later of acceptance and the last snapshot — never from a snapshot that
    /// predates acceptance, so a stream already near-stale when the operator hit
    /// Reload can't bank that pre-acceptance quiet against the respawn wait. A
    /// post-acceptance snapshot (lastUpdate > acceptedAt) moves the anchor forward;
    /// a daemon that dies at acceptance and never returns trips it `bound` after
    /// acceptance. Static so it's unit-testable without a live stream.
    nonisolated static func respawnSilenceExpired(
        acceptedAt: Date, lastUpdate: Date?, now: Date, bound: TimeInterval
    ) -> Bool {
        let anchor = max(acceptedAt, lastUpdate ?? acceptedAt)
        return now.timeIntervalSince(anchor) > bound
    }

    /// Pure: turns a refused ReloadResponse into the operator-facing banner —
    /// the failed checks, plus the loud warning when a drain is already running
    /// and WILL load the invalid file. Static so it's unit-testable.
    nonisolated static func describeRefusal(_ resp: Runny_V1_ReloadResponse) -> String {
        var lines = [
            "reload refused — the new config failed validation; the running daemon is unchanged",
        ]
        for check in resp.failedChecks where !check.ok {
            lines.append("• \(check.name): \(check.detail)")
        }
        if !resp.draining.isEmpty {
            lines.append(
                "WARNING: the daemon is already draining (\(resp.draining)) and the "
                    + "respawn WILL load this invalid config — fix it before the drain converges"
            )
        }
        return lines.joined(separator: "\n")
    }

    /// Pure: the whole respawn taxonomy against the validated config, mirroring
    /// runnyctl's `respawnVerdict`. Static so every branch is unit-testable. A
    /// `.failure` is config drift (the operator must act); the job-in-flight case
    /// is a `.warning` (the config IS live, but a job may have been interrupted).
    nonisolated static func respawnVerdict(
        protocolVersion: UInt32, gotSHA: String, wantSHA: String,
        jobInFlight: Bool, reDraining: String
    ) -> ReloadOutcome {
        let want = shortSHA(wantSHA)
        let note = reDraining.isEmpty
            ? "" : " (the new daemon is already draining again: \(reDraining))"
        if protocolVersion < 2 || gotSHA.isEmpty {
            return ReloadOutcome(
                text: "daemon respawned, but it doesn't report its running config hash — "
                    + "can't verify it came up on \(want); upgrade runnyd to confirm\(note)",
                severity: .warning
            )
        }
        if gotSHA != wantSHA {
            return ReloadOutcome(
                text: "daemon respawned on config \(shortSHA(gotSHA)), NOT the config you "
                    + "reloaded (\(want)) — the on-disk file changed during the drain",
                severity: .failure
            )
        }
        if jobInFlight {
            return ReloadOutcome(
                text: "daemon respawned on config \(want), but the previous daemon went down "
                    + "with a job still running — it may have been interrupted\(note)",
                severity: .warning
            )
        }
        return ReloadOutcome(
            text: "reloaded: respawned on config \(want)\(note)", severity: .success
        )
    }

    nonisolated static func shortSHA(_ s: String) -> String {
        s.count > 12 ? String(s.prefix(12)) : s
    }

    // MARK: - Version skew (warn, never refuse)

    /// The `x.y.z` core of a version string — the leading `\d+.\d+.\d+`, or nil if
    /// the string doesn't start with one. The daemon publishes its full build
    /// label (`0.6.0-beta.<sha>`) while the app's bundle version is already
    /// stripped to its core by the build, so normalizing both sides to the core
    /// before comparing keeps a same-commit beta pair from false-alarming. The
    /// match is anchored at the start, mirroring the build's `re.match` capture, so
    /// a label that doesn't begin with `x.y.z` (empty, a dev label, an unexpected
    /// prefix) yields nil → quiet rather than mis-extracting a triple from
    /// somewhere in the middle.
    nonisolated static func versionCore(_ s: String) -> String? {
        guard let range = s.range(of: #"^\d+\.\d+\.\d+"#, options: .regularExpression)
        else { return nil }
        return String(s[range])
    }

    /// Pure: the version-skew verdict between this app and the daemon it watches,
    /// or nil when they match, the daemon's version isn't known yet, the app is an
    /// unstamped dev build, or the daemon is merely newer (the safe monotone
    /// direction). Static and parameterized on the four facts — never reading
    /// `Bundle.main` — so every branch is unit-testable without a live daemon.
    ///
    /// Two independent axes, neither implied by the other:
    ///  - `versionMismatch`: the normalized `x.y.z` cores differ — the shared-host
    ///    brew-daemon-at-another-release case. Symmetric.
    ///  - `protocolBehind`: the cores match but the daemon's protocol is below what
    ///    this app's wire stubs expect — the new-app/old-daemon upgrade window,
    ///    invisible to the version axis (same `x.y.z`) and the ONLY detector for it.
    nonisolated static func skewVerdict(
        appVersion: String, appExpectedProtocol: UInt32,
        daemonVersion: String, daemonProtocol: UInt32
    ) -> SkewVerdict? {
        // No version heard from the daemon yet (fresh connect, or a daemon
        // predating the field): never warn about a version we don't have.
        guard let daemonCore = versionCore(daemonVersion) else { return nil }
        // An unstamped dev build — or a missing bundle key coalesced to the
        // unstamped sentinel — must not wear a permanent false banner. It accepts
        // that a dev build could miss a real skew; a dev build is never a shipped
        // install.
        guard let appCore = versionCore(appVersion), appCore != unstampedVersion
        else { return nil }
        // Different release lines — the shared-host / lagging-channel case. Name
        // the normalized cores, not the daemon's full suffix-bearing string: a
        // same-core rebuild that only rotates the build sha must not change the
        // verdict and re-pop a dismissed banner. The full daemon version is shown
        // in the version line above either surface.
        if appCore != daemonCore {
            return SkewVerdict(
                kind: .versionMismatch,
                text: "this app is \(appCore) but the daemon is \(daemonCore) — "
                    + "different releases; upgrade the lagging install"
            )
        }
        // Same release, but the daemon predates a capability this app's stubs
        // expect — the upgrade window the matched cores hide. `<`, not `!=`: a
        // newer daemon serving an older-expecting app degrades nothing.
        if daemonProtocol < appExpectedProtocol {
            return SkewVerdict(
                kind: .protocolBehind,
                text: "the running daemon predates a capability this app expects — "
                    + "some features may not work; upgrade or restart runnyd"
            )
        }
        return nil
    }

    /// Pure: is the app a strictly newer build than the daemon? The direction the
    /// symmetric skew verdict doesn't compute. False for an unstamped dev app (it
    /// can't meaningfully "update" anything) or a daemon with no version yet.
    nonisolated static func appNewerThanDaemon(appVersion: String, daemonVersion: String) -> Bool {
        guard let app = versionCore(appVersion), app != unstampedVersion,
              let daemon = versionCore(daemonVersion)
        else { return false }
        return semverGreater(app, daemon)
    }

    /// Pure: numeric (not lexical) compare of two `x.y.z` cores — so 0.10.0 > 0.9.0.
    nonisolated static func semverGreater(_ a: String, _ b: String) -> Bool {
        let pa = a.split(separator: ".").map { Int($0) ?? 0 }
        let pb = b.split(separator: ".").map { Int($0) ?? 0 }
        for i in 0 ..< max(pa.count, pb.count) {
            let x = i < pa.count ? pa[i] : 0
            let y = i < pb.count ? pb[i] : 0
            if x != y { return x > y }
        }
        return false
    }

    /// Pure: same-core-older-protocol — the upgrade window the version compare
    /// alone misses (e.g. a beta/rebuild whose stubs expect a newer protocol). A
    /// reload moves launchd onto the bundled binary, so it IS update-eligible for
    /// an app-installed agent. Mirrors `skewVerdict`'s protocol axis.
    nonisolated static func protocolBehind(
        appVersion: String, daemonVersion: String, daemonProtocol: UInt32, appExpectedProtocol: UInt32
    ) -> Bool {
        guard let app = versionCore(appVersion), app != unstampedVersion,
              let daemon = versionCore(daemonVersion), app == daemon
        else { return false }
        return daemonProtocol < appExpectedProtocol
    }

    /// Pure: the daemon-update surface. Offered ONLY for an app-installed agent the
    /// app is ahead of on EITHER axis — a newer version core, or the same core with
    /// an older protocol (a reload picks up the bundled binary either way). A
    /// brew/manual daemon would drain its fleet for a respawn of the same binary, so
    /// it never sees this. While the update reload drains, `inProgress`; after it
    /// resolves still-behind, `didNotTake` (named, loud).
    nonisolated static func daemonUpdate(
        agentInstalled: Bool, agentCanonical: Bool, runningBundleCanonical: Bool,
        appNewer: Bool, protocolBehind: Bool, daemonCore: String,
        reloadPending: Bool, attempted: Bool
    ) -> DaemonUpdate {
        // agentCanonical: the registered job points at THIS app's /Applications
        // bundle (a reload respawns it). runningBundleCanonical: the RUNNING bundle
        // IS that /Applications app — so the appNewer comparison reflects the binary
        // the reload will actually respawn. Both are required: a newer app run from
        // Downloads (running bundle not canonical) reads as appNewer, but the reload
        // respawns the older /Applications binary, so the update could never take.
        guard agentInstalled, agentCanonical, runningBundleCanonical, appNewer || protocolBehind
        else { return .none }
        if reloadPending { return .inProgress }
        if attempted { return .didNotTake(daemonCore: daemonCore) }
        return .available
    }

    /// Pure: must the uninstall raise the abandon confirmation? Yes whenever a live
    /// guest is present OR the live-guest state is UNKNOWN — a disconnected or
    /// pre-first-snapshot store reports an empty list that means "no snapshot", not
    /// "no guest", so an empty list is safe to skip only while connected.
    nonisolated static func uninstallNeedsConfirmation(connected: Bool, liveGuestSlots: [String]) -> Bool {
        !liveGuestSlots.isEmpty || !connected
    }

    /// Pure: the skew to actually render, applying the two visibility gates that
    /// keep the detector from itself failing silently. Static so both are
    /// unit-testable without a live store.
    ///  - Connection gate: on a drop/stale/unreachable transition the supervisor
    ///    flips `connection` WITHOUT calling `apply()`, so a stored `skew` would
    ///    linger and assert skew about a daemon that may have recycled — show
    ///    nothing unless the connection is live.
    ///  - Dismiss gate: suppress a skew the operator dismissed, keyed on the full
    ///    `Equatable` verdict, so a worsening or different-axis skew on the same
    ///    version string is new news and re-surfaces.
    nonisolated static func gatedSkew(
        skew: SkewVerdict?, connection: ConnectionState, dismissed: SkewVerdict?
    ) -> SkewVerdict? {
        guard connection == .connected, let skew, skew != dismissed else { return nil }
        return skew
    }
}
