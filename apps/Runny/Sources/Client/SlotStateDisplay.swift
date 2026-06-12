import RunnyV1
import SwiftUI

extension Runny_V1_SlotState {
    /// Cycle order, NOT proto numeric order: SECURE_SSH was appended for wire
    /// compat and sits between AWAIT_SSH and MINT_JIT (see the proto comment).
    /// Render and sort by this, never by rawValue.
    static let cycleOrder: [Runny_V1_SlotState] = [
        .backoff, .ensureImage, .clone, .boot, .awaitIp, .awaitSsh,
        .secureSsh, .mintJit, .provision, .listening, .job, .teardown,
    ]

    /// Position in the cycle, for sorting state records. Unknown/unspecified
    /// states sort last rather than breaking the timeline.
    var cycleIndex: Int {
        Self.cycleOrder.firstIndex(of: self) ?? Self.cycleOrder.count
    }

    /// Display name matching runnyctl: enum name with the prefix stripped.
    var displayName: String {
        switch self {
        case .unspecified: "—"
        case .backoff: "BACKOFF"
        case .ensureImage: "ENSURE_IMAGE"
        case .clone: "CLONE"
        case .boot: "BOOT"
        case .awaitIp: "AWAIT_IP"
        case .awaitSsh: "AWAIT_SSH"
        case .secureSsh: "SECURE_SSH"
        case .mintJit: "MINT_JIT"
        case .provision: "PROVISION"
        case .listening: "LISTENING"
        case .job: "JOB"
        case .teardown: "TEARDOWN"
        case let .UNRECOGNIZED(n): "STATE(\(n))"
        }
    }

    var tint: Color {
        switch self {
        case .listening: .green
        case .job: .blue
        case .backoff: .secondary.opacity(0.8)
        case .teardown: .orange
        case .unspecified, .UNRECOGNIZED: .secondary
        default: .yellow // the transient provisioning states
        }
    }
}
