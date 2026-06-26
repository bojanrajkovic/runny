import SwiftUI

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
        Task { await startGatedReload(store, agent) }
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
        agentInstalled: agent.ownership == .selfManaged && agent.installState == .installed,
        agentCanonical: agent.reconcileState == .ok,
        runningBundleCanonical: agent.eligibility == .eligible
    )
}

/// Whether a plain Reload right now might respawn a newer bundled binary — the
/// signal `startGatedReload` uses to choose the config-compat gate over a plain
/// reload. Two legs, in order:
///
/// 1. **Is the app even ahead?** A reload can only upgrade if the bundled binary is
///    newer than the running daemon — a pure version/protocol compare
///    (`appAheadOfDaemon`) that does NOT depend on the ownership/reconcile facts. If
///    the app isn't ahead (same version, an older/translocated build, or a dev build
///    with no bundled `runnyd`), a reload can't upgrade, so it must NOT be gated on
///    the bundled probe — an older or missing bundled binary would falsely block a
///    config the running daemon happily accepts.
/// 2. **Given it's ahead, is the respawn ours?** FAIL CLOSED while the agent facts
///    aren't a settled, affirmative verdict — `ownership == .indeterminate`, or
///    `reconcileState` `.notChecked` (not yet run) or `.undetermined` (wedged/
///    unparseable `launchctl`): the reload could respawn the newer bundled daemon, so
///    gate it or it crash-loops on a schema-incompatible config. Only once reconcile
///    lands a real verdict (`.ok`/`.foreign`) is `daemonUpdateVerdict` authoritative.
@MainActor
func reloadMightUpgrade(_ store: DaemonStore, _ agent: AgentController) -> Bool {
    guard store.appAheadOfDaemon else { return false }
    if agent.ownership == .indeterminate
        || agent.reconcileState == .notChecked
        || agent.reconcileState == .undetermined
    {
        return true
    }
    return daemonUpdateVerdict(store, agent) != .none
}

/// The single entry point both the Update Daemon affordance and the plain Reload
/// buttons call. A reload that can't upgrade (the app isn't ahead, or doesn't own
/// the agent) is a plain config reload — generic confirm, the daemon's own preflight
/// validates it. A reload that WOULD upgrade re-gathers ownership first (the button
/// can be stale, and an upgrade fires outside the spawn gate, so a daemon that
/// changed hands must not be drained) and then runs the config-compat gate, which
/// surfaces OK → reload now / Warn → confirm popup / Error → block popup. If
/// ownership slipped to foreign/system, fall back to a plain reload rather than
/// silently doing nothing.
@MainActor
func startGatedReload(_ store: DaemonStore, _ agent: AgentController) async {
    guard reloadMightUpgrade(store, agent) else {
        store.requestReload()
        return
    }
    if await agent.revalidate(.selfManaged), agent.installState == .installed {
        await store.gatedDaemonUpdate()
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
