import RunnyV1
import SwiftUI

extension Runny_V1_SlotState {
    /// Cycle order, NOT proto numeric order: SECURE_SSH (12) and DEBUG (13)
    /// were appended for wire compat but sit mid-cycle (see the proto
    /// comments). DEBUG is the operator hold between JOB and TEARDOWN.
    /// Render and sort by this, never by rawValue.
    static let cycleOrder: [Runny_V1_SlotState] = [
        .backoff, .ensureImage, .clone, .boot, .awaitIp, .awaitSsh,
        .secureSsh, .mintJit, .provision, .listening, .job, .debug, .teardown,
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
        case .debug: "DEBUG"
        case .teardown: "TEARDOWN"
        case let .UNRECOGNIZED(n): "STATE(\(n))"
        }
    }

    var tint: Color {
        switch self {
        case .listening: .green
        case .job: .blue
        case .debug: .purple // operator hold — runner killed, max-idle suspended
        case .backoff: .secondary.opacity(0.8)
        case .teardown: .orange
        case .unspecified, .UNRECOGNIZED: .secondary
        default: .yellow // the transient provisioning states
        }
    }
}

extension Runny_V1_SlotStatus {
    /// The one slot-health color rule: wedged overrides everything.
    var effectiveTint: Color {
        wedged ? .red : state.tint
    }
}
