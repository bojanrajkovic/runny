import SwiftUI

/// The daemon's own log. Per-runner output lives on each runner's Logs tab,
/// so there's no fleet "runner output" mode here — that was a duplicate of
/// what the runner views already show.
struct FleetLogsPane: View {
    var body: some View {
        VStack(spacing: 0) {
            HStack {
                Text("Daemon log")
                    .font(.title2)
                    .fontWeight(.semibold)
                Spacer()
            }
            .padding(.horizontal)
            .padding(.top, 14)
            .padding(.bottom, 6)
            Divider()
            LogsTab(slotName: nil, daemon: true)
        }
        .ignoresSafeArea(.container, edges: .top)
    }
}
