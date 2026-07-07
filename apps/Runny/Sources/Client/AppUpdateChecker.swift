import Foundation

/// Bounded, fail-quiet GitHub releases/latest poller. Every error path
/// (network error, non-200, 403 rate-limit, bad JSON, unparseable tag)
/// returns nil — silence is always better than a false "you're behind" banner.
enum AppUpdateChecker {
    private struct LatestRelease: Decodable {
        let tag_name: String
        let html_url: String
    }

    /// `fetcher` is the network call, injectable for tests — same seam shape
    /// as DaemonStore.clientFactory: defaults to the real URLSession round
    /// trip, tests substitute canned (data, response) or a thrown error,
    /// exercising the status-code/JSON/tag parsing this function actually
    /// does without a stubbed HTTP stack.
    static func fetch(
        appVersion: String,
        fetcher: (URLRequest) async throws -> (Data, URLResponse) = { try await URLSession.shared.data(for: $0) }
    ) async -> DaemonStore.AppUpdate? {
        guard
            let url = URL(
                string: "https://api.github.com/repos/bojanrajkovic/runny/releases/latest"
            )
        else { return nil }
        var request = URLRequest(url: url, timeoutInterval: 10)
        request.setValue("application/vnd.github+json", forHTTPHeaderField: "Accept")
        request.setValue("2022-11-28", forHTTPHeaderField: "X-GitHub-Api-Version")
        do {
            let (data, response) = try await fetcher(request)
            guard let http = response as? HTTPURLResponse, http.statusCode == 200
            else { return nil }
            let release = try JSONDecoder().decode(LatestRelease.self, from: data)
            guard
                let newVersion = DaemonStore.releaseNewerThanApp(
                    appVersion: appVersion, latestTag: release.tag_name
                ),
                let pageURL = URL(string: release.html_url)
            else { return nil }
            return DaemonStore.AppUpdate(version: newVersion, url: pageURL)
        } catch {
            return nil
        }
    }
}

extension NSNotification.Name {
    /// Posted by the "Check for Updates…" menu command to trigger an
    /// immediate check outside the 24h timer cycle.
    static let runnyCheckForAppUpdates = NSNotification.Name("com.coderinserepeat.runny.checkForAppUpdates")
}
