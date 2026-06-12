import SwiftUI

/// Fleet-wide logs: all runners interleaved, or the daemon's own log.
/// The two are distinct server streams and mutually exclusive by contract,
/// so the picker swaps the whole view (and its stream) rather than filtering.
struct FleetLogsPane: View {
    enum Mode: String, CaseIterable {
        case runners = "Runner Output"
        case daemon = "Daemon Log"
    }

    @State private var mode: Mode = .runners

    var body: some View {
        VStack(spacing: 0) {
            Picker("", selection: $mode) {
                ForEach(Mode.allCases, id: \.self) { Text($0.rawValue) }
            }
            .pickerStyle(.segmented)
            .labelsHidden()
            .padding(10)
            Divider()
            // id(mode) tears down the old stream with its view.
            LogsTab(slotName: nil, daemon: mode == .daemon)
                .id(mode)
        }
    }
}
