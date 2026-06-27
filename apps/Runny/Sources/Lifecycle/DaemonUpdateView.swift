import SwiftUI
import UserNotifications

/// UserDefaults keys for the app's preferences.
enum Prefs {
    /// Default-on "automatically apply runnyd upgrades". Read by the surface-driven
    /// auto-apply trigger and bound by the Settings toggle.
    static let autoApplyDaemonUpdates = "autoApplyDaemonUpdates"
}

/// The post-upgrade daemon-update affordance for the menu bar and main window.
/// Shown only when the app installed the agent AND is the newer build: Update
/// issues a drain-gated reload (jobs finish first, then launchd cold-starts the
/// freshly-bundled binary). A non-converged result is named loud — "still vX" —
/// not folded into the generic reload note. Self-hides otherwise.
struct DaemonUpdateAffordance: View {
    @Environment(DaemonStore.self) private var store
    @Environment(AgentController.self) private var agent
    @Environment(ActivationCoordinator.self) private var activation
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        // The gate verdict is surfaced as popups (ConfigGateAlerts), not inline rows —
        // a row rendered behind the modal reload prompt was easy to miss.
        updateAffordance(daemonUpdateVerdict(store, agent))
    }

    @ViewBuilder private func updateAffordance(_ verdict: DaemonStore.DaemonUpdate) -> some View {
        switch verdict {
        case .none:
            EmptyView()
        case .available:
            AffordanceRow(
                systemImage: icon,
                text: "A newer runnyd ships with this app. Update drains running jobs first, then restarts.",
                tint: .orange
            ) {
                Button("Update Daemon") { update() }
            }
        case .inProgress:
            AffordanceRow(systemImage: icon, text: "Updating runnyd — draining running jobs first…", tint: .secondary) {
                ProgressView().controlSize(.small)
            }
        case let .didNotTake(core):
            AffordanceRow(systemImage: icon, text: "Update didn't take — runnyd is still \(core).", tint: .red) {
                Button("Try Again") { update() }
            }
        }
    }

    private let icon = "arrow.down.circle"

    /// The gate's popups are hosted on the main window (the popover panel has no
    /// reliable presenter), so open it first — a no-op refocus from the window itself,
    /// load-bearing from the popover.
    private func update() {
        activation.openMainWindow(openWindow)
        Task { await startGatedReload(store, agent, explicitUpdate: true) }
    }
}

/// The daemon-update verdict for the current store + agent — shared by the update
/// affordance and by the plain Reload buttons (which must gate a reload that would
/// respawn the newer bundled binary). It requires BOTH ownership and installState,
/// each guarding a distinct staleness: `ownership == .selfManaged` rejects a verdict
/// the app doesn't drain-update (a `systemManaged` daemon, or any deferring verdict);
/// `installState == .installed` rejects a stale `.selfManaged` left by a teardown the
/// verdict didn't re-gather. The canonical checks (`reconcileState == .ok`,
/// `eligibility == .eligible`) require AFFIRMATIVE confirmation that the registered
/// agent and the running bundle are THIS `/Applications` app, so the verdict reflects
/// the binary a reload would actually respawn — never a translocated or foreign one.
/// `!= .none` therefore means "a reload right now would cold-start a newer bundled
/// daemon", the precise condition under which a plain reload is really an update.
@MainActor
func daemonUpdateVerdict(_ store: DaemonStore, _ agent: AgentController) -> DaemonStore.DaemonUpdate {
    store.daemonUpdate(
        agentInstalled: agent.isSelfManagedInstalled,
        agentCanonical: agent.reconcileState == .ok,
        runningBundleCanonical: agent.eligibility == .eligible
    )
}

/// Whether a reload would respawn our NEWER bundle, and so must run the config-compat
/// gate. **Ownership decides it; the version compare is a trusted-only-from-canonical
/// short-circuit:**
///
/// - **Ours?** The reload must respawn OUR bundle, which only the *owner* determines:
///   a `selfManaged` daemon is our per-user agent, so a reload cold-starts our
///   `BundleProgram` — gate it. `indeterminate` (a wedged system probe) can't be told
///   apart from ours, so fail closed. A settled non-self owner respawns its OWN binary,
///   validated by its own reload preflight — not ours to gate.
/// - **Ahead?** A reload can only *upgrade* if the respawn binary is newer — but the
///   respawn binary is the canonical `/Applications` one, and `appAheadOfDaemon` reads
///   the RUNNING bundle's version. That's only the same thing when the running bundle IS
///   canonical (`eligibility == .eligible`). So "not ahead → skip the gate" is trusted
///   only then; from a stray/translocated copy the running version is meaningless to the
///   respawn, so fail closed (don't skip) and let ownership gate it onto the canonical
///   binary it actually probes.
///
/// Reconcile and the affordance verdict still don't enter (a `selfManaged` daemon
/// respawns our `BundleProgram` regardless) — the model stays ownership + a
/// canonical-trusted version short-circuit, not a per-cell cross product.
@MainActor
func reloadMightUpgrade(_ store: DaemonStore, _ agent: AgentController) -> Bool {
    if agent.eligibility == .eligible, !store.appAheadOfDaemon { return false }
    switch agent.ownership {
    // ponytail: `.indeterminate` fails closed though it's usually a foreign/system
    // daemon — accepting a rare, transient, retryable false-block, because the
    // alternative is a crash-loop if it really is our per-user agent, and
    // crash-loop-proof beats false-block.
    case .selfManaged, .indeterminate:
        return true
    case .unmanaged, .systemManaged, .awaitingApproval:
        return false
    }
}

/// The single entry point both reload affordances call. Re-gather ownership first —
/// the button can be stale, and a reload drains the fleet, so a daemon that changed
/// hands must not be drained. Then the two entry points part on their *inherent*
/// ownership strictness:
///
/// - **Update Daemon** (`explicitUpdate: true`) means "upgrade MY per-user daemon", so
///   it proceeds ONLY when ownership is confirmed `.selfManaged` and installed — the
///   original `update()` guard. Indeterminate (a wedged probe), foreign/system, or
///   uninstalled → **refuse**, never drain a daemon we can't confirm is ours; the
///   affordance re-renders and self-hides. The click is the drain consent, so OK reloads
///   immediately.
/// - **Reload Config** (`explicitUpdate: false`) means "reload the CONNECTED daemon", so
///   it gates whenever the reload might upgrade — including indeterminate ownership
///   (crash-loop-proof) — and otherwise does a plain reload; either way its drain dialog
///   is the consent. Crucially it never silently drains.
@MainActor
func startGatedReload(_ store: DaemonStore, _ agent: AgentController, explicitUpdate: Bool) async {
    let confirmedOurs = await agent.confirmedSelfManaged()
    if explicitUpdate {
        if confirmedOurs { await store.gatedDaemonUpdate(explicitUpdate: true) }
    } else if reloadMightUpgrade(store, agent) {
        await store.gatedDaemonUpdate(explicitUpdate: false)
    } else {
        store.requestReload()
    }
}

/// The config-compat gate's popups, hosted on the main window root alongside the
/// command alerts and the generic reload confirm. The gate verdict IS the prompt:
/// a Warn presents a confirm-or-cancel alert (Cancel is the safe default; "Reload
/// Anyway" is the destructive, deliberate action); an Error presents an
/// acknowledge-only alert that reloads nothing. OK shows no popup — it reloads
/// straight away, since clicking Update/Reload was already the consent.
struct ConfigGateAlerts: ViewModifier {
    @Environment(DaemonStore.self) private var store

    func body(content: Content) -> some View {
        content
            .alert("Config has warnings", isPresented: warnPresented) {
                Button("Reload Anyway", role: .destructive) { store.confirmGatedUpdate() }
                Button("Cancel", role: .cancel) { store.clearConfigGate() }
            } message: {
                Text(warnMessage)
            }
            .alert("Can’t update runnyd", isPresented: blockPresented) {
                Button("OK", role: .cancel) { store.clearConfigGate() }
            } message: {
                Text("The newer runnyd rejects the current config, so nothing was changed:\n\n\(store.configGateBlock ?? "")")
            }
    }

    private var warnPresented: Binding<Bool> {
        Binding(get: { !store.configGateWarnings.isEmpty }, set: { if !$0 { store.clearConfigGate() } })
    }

    private var blockPresented: Binding<Bool> {
        Binding(get: { store.configGateBlock != nil }, set: { if !$0 { store.clearConfigGate() } })
    }

    private var warnMessage: String {
        let lines = store.configGateWarnings.map { "• \($0.message)" }.joined(separator: "\n")
        return "The newer runnyd accepts this config but flagged:\n\n\(lines)\n\nReload onto it anyway?"
    }
}

extension View {
    func configGateAlerts() -> some View { modifier(ConfigGateAlerts()) }
}

/// The surface-driven auto-apply trigger. Run (by `AutoApplyOnAppear`) when the update
/// verdict settles to `.available` while a Runny surface is open — so the operator is
/// present when the fleet drains. Fires only when the default-on setting is enabled, an
/// update is on offer, none's been attempted this cycle
/// (`autoApplyShouldAttempt`), ownership re-confirms `.selfManaged` + installed (never
/// auto-drain a daemon we don't own), and the config gate returns OK (`autoApplyOnOK`
/// — Warn/Error leave the manual Update affordance for a deliberate click). On a fired
/// auto-apply it posts the notification, since the drain happened without a click.
@MainActor
func maybeAutoApply(_ store: DaemonStore, _ agent: AgentController, settingOn: Bool) async {
    guard DaemonStore.autoApplyShouldAttempt(
        settingOn: settingOn,
        update: daemonUpdateVerdict(store, agent),
        attempted: store.daemonUpdateAttempted
    ) else { return }
    guard await agent.confirmedSelfManaged() else { return }
    // daemonVersion may be a sha-bearing build label; appVersion is already its bare
    // core (build-stripped), so normalize only the daemon side before the notice.
    let from = DaemonStore.versionCore(store.daemonVersion) ?? store.daemonVersion
    if await store.autoApplyOnOK() {
        AutoApplyNotifier.notifyApplying(from: from, to: DaemonStore.appVersion)
    }
}

/// The surface-driven auto-apply trigger as one modifier, applied to both the menu-bar
/// popover and the main window so the gather and the default-on setting live in ONE
/// place, not copy-pasted per surface.
///
/// Two halves, because the update verdict isn't ready when the surface appears: the
/// `.task` gathers ALL three of the verdict's agent facts (installState via `refresh`,
/// reconcileState via `runReconcile`, ownership via `refreshOwnership`) on appear — so
/// the trigger doesn't lean on the foreground observer happening to have refreshed
/// ownership, which a popover-only open need not have. The verdict ALSO needs the live
/// daemon connection — `daemonUpdate` is `.none` until a status snapshot lands, and that
/// snapshot arrives asynchronously after appear. So firing once in the `.task` races the
/// connection and loses. Instead `.onChange(…, initial: true)` re-evaluates whenever the
/// verdict changes, firing auto-apply the moment it settles to `.available` (whether
/// that's already true at appear or lands a beat later). `maybeAutoApply` re-checks
/// eligibility, so non-`.available` transitions are no-ops; and the nudge goes through
/// `store.considerAutoApply`, which single-flights — both surfaces firing on one settle
/// collapse to a single attempt, so there's no per-surface race to guard.
struct AutoApplyOnAppear: ViewModifier {
    @Environment(DaemonStore.self) private var store
    @Environment(AgentController.self) private var agent
    @AppStorage(Prefs.autoApplyDaemonUpdates) private var autoApplyDaemonUpdates = true

    func body(content: Content) -> some View {
        content
            .task {
                agent.refresh()
                await agent.runReconcile()
                await agent.refreshOwnership()
            }
            .onChange(of: daemonUpdateVerdict(store, agent), initial: true) {
                store.considerAutoApply {
                    await maybeAutoApply(store, agent, settingOn: autoApplyDaemonUpdates)
                }
            }
    }
}

extension View {
    func autoApplyOnAppear() -> some View { modifier(AutoApplyOnAppear()) }
}

/// Best-effort local notification when auto-apply drains+restarts the fleet without a
/// click. Authorization is requested lazily on first post; if denied (or on an
/// ad-hoc/unnotarized build where it's flaky), it stays silent — the affordance still
/// shows the in-progress/outcome state, so a missing notification is never a missing
/// signal.
enum AutoApplyNotifier {
    static func notifyApplying(from: String, to: String) {
        let center = UNUserNotificationCenter.current()
        center.requestAuthorization(options: [.alert]) { granted, _ in
            guard granted else { return }
            let content = UNMutableNotificationContent()
            content.title = "Applying runnyd update"
            // `to` (the app version) never coalesces to empty. When `from == to` — a
            // protocol-only upgrade (same version core, older protocol) — don't render
            // "0.6.0 → 0.6.0"; just name the target.
            content.body = (from.isEmpty || from == to)
                ? "Updating runnyd to \(to) and restarting the fleet (running jobs finish first)."
                : "Updating runnyd \(from) → \(to) and restarting the fleet (running jobs finish first)."
            center.add(UNNotificationRequest(identifier: "runnyd-auto-apply", content: content, trigger: nil))
        }
    }
}
