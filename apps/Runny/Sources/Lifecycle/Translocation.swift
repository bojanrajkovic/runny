import Foundation

/// Whether a bundle is translocated or at a transient path — a copy that would
/// evaporate on the next launch. Used by the per-user agent's install eligibility:
/// registering a LaunchAgent pointing into a translocated bundle breaks when the
/// bundle disappears. Not privileged — a plain path heuristic.
///
/// The Security SPI that answers translocation authoritatively isn't in the Swift
/// import surface, so this matches the App Translocation mount root Gatekeeper
/// always uses (no false negatives for the actual hazard) and fails closed for any
/// `/private/var/folders` transient path.
enum Translocation {
    static func isTranslocated(_ bundlePath: String) -> Bool {
        bundlePath.contains("/AppTranslocation/") || bundlePath.hasPrefix("/private/var/folders/")
    }
}
