import Foundation

/// One-time cleanup of UserDefaults keys the app no longer reads.
///
/// When a Settings field or preference is removed, the key it wrote lingers in
/// the user's defaults plist — inert, but orphaned state. `prune` drops the
/// known-retired keys so the plist doesn't accumulate dead entries across
/// upgrades. It is idempotent: removing an absent key is a no-op, so it runs
/// unconditionally at launch with no "have I already migrated?" flag to keep in
/// sync.
enum RetiredDefaults {
    /// Keys written by a since-removed surface. Append here whenever a
    /// preference is retired; never reuse one of these names for a live key.
    static let keys = [
        // The "Runny home" Settings override, removed when the home was fixed
        // at ~/.runny — RunnyHome no longer reads it, so a value left here is
        // dead weight (and, before the fix, a silent app↔daemon split-brain).
        "runnyHomeOverride",
    ]

    static func prune(_ defaults: UserDefaults = .standard) {
        for key in keys {
            defaults.removeObject(forKey: key)
        }
    }
}
