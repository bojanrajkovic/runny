import Foundation
import RunnyV1

/// The pure, `nonisolated static` verdict layer `DaemonStore` drains into: reload
/// outcomes (respawn taxonomy, refusal banners, drain-stall detection) and
/// version/protocol skew (app↔daemon comparison, semver core parsing, the
/// daemon-update and auto-apply gates). Split out of `DaemonStore.swift` as a
/// mechanical file move — no logic changes — because these are pure functions of
/// their parameters, never touching `self`, unlike the instance methods
/// (`trackReloadDrain`, `noteRespawnIfReady`, `checkReloadRespawnDeadline`,
/// `isReloadSuccessor`, the app-update poll loop) that stay in `DaemonStore.swift`
/// alongside the state they read and write.
extension DaemonStore {
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
        let want = String(wantSHA.prefix(12))
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
                text: "daemon respawned on config \(String(gotSHA.prefix(12))), NOT the config you "
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

    /// Pure: is `latestTag` (the GitHub API's `tag_name`, e.g. `"v0.7.0"`) a
    /// release strictly newer than `appVersion`? Returns the normalized `x.y.z`
    /// core of the release if it is, nil otherwise. Fail-quiet: an unstamped app
    /// (`0.0.0`), an unparseable tag, or an equal/older release all return nil.
    /// Strips a leading `v` before normalizing; handles `-beta.<sha>` suffixes via
    /// `versionCore`'s anchored match — the same normalization the skew detector uses.
    nonisolated static func releaseNewerThanApp(appVersion: String, latestTag: String) -> String? {
        let tag = latestTag.hasPrefix("v") ? String(latestTag.dropFirst()) : latestTag
        guard
            let latestCore = versionCore(tag),
            let appCore = versionCore(appVersion), appCore != unstampedVersion
        else { return nil }
        return semverGreater(latestCore, appCore) ? latestCore : nil
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
    /// `.numeric` compares each dot-separated run of digits as a number, which is
    /// exactly the regex-normalized triple this always receives.
    nonisolated static func semverGreater(_ a: String, _ b: String) -> Bool {
        a.compare(b, options: .numeric) == .orderedDescending
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

    /// Pure: whether to ATTEMPT auto-apply — the cheap precondition checked before
    /// the async revalidate + config-compat probe. Fires only when the default-on
    /// setting is enabled, an update is actually on offer (`.available`), and none has
    /// been attempted this cycle. `attempted` (`daemonUpdateAttempted`) is the loop
    /// backstop: a non-converged update leaves it set, so a `didNotTake` drops to the
    /// manual "Try Again" rather than an auto-retry drain loop. The OK-only gate (Warn/
    /// Error never auto-apply) and the confirmed-`.selfManaged` ownership check happen
    /// after this, in the trigger.
    nonisolated static func autoApplyShouldAttempt(settingOn: Bool, update: DaemonUpdate, attempted: Bool) -> Bool {
        settingOn && update == .available && !attempted
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
