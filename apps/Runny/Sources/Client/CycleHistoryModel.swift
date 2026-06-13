import Foundation
import Observation
import RunnyV1

/// Why-backed cycle history for one slot. Completed cycles only — cycle.json
/// is written at cycle end, so the in-flight cycle never appears here; the
/// live position is the detail header's job. Never reconstructed from
/// WatchStatus (snapshots coalesce and skip states).
@MainActor
@Observable
final class CycleHistoryModel {
    private(set) var cycles: [Runny_V1_CycleRecord] = []
    private(set) var loading = false
    private(set) var loadError: String?

    static let depth: UInt32 = 20

    /// The cycle_id this history was last fetched against; refetch when the
    /// slot moves on.
    private var fetchedForCycle: String?
    private var inFlight: Task<Void, Never>?

    func refreshIfNeeded(slot: Runny_V1_SlotStatus, store: DaemonStore) {
        // Keyed on the cycle we last fetched for, NOT on emptiness: a slot with
        // genuinely no completed cycles fetched-and-got-nothing, so re-fetching
        // every cycle would just re-confirm empty (fetchedForCycle is only set
        // on a successful fetch, so a load error still retries).
        guard fetchedForCycle != slot.cycleID else { return }
        refresh(slotName: slot.slot, cycleID: slot.cycleID, store: store)
    }

    func refresh(slotName: String, cycleID: String, store: DaemonStore) {
        guard let client = store.client else {
            loadError = "daemon unreachable"
            return
        }
        inFlight?.cancel()
        loading = true
        loadError = nil
        inFlight = Task { @MainActor [weak self] in
            // A cancelled predecessor must not clear the successor's spinner.
            defer { if !Task.isCancelled { self?.loading = false } }
            do {
                let records = try await client.why(slot: slotName, cycles: Self.depth)
                guard !Task.isCancelled else { return }
                self?.cycles = records
                self?.fetchedForCycle = cycleID
            } catch {
                guard !Task.isCancelled else { return }
                self?.loadError = "couldn't load cycles: \(error.localizedDescription)"
            }
        }
    }
}
