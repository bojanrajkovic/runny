import XCTest

@testable import Runny

/// The app-update notify path: `releaseNewerThanApp` is the pure decision
/// layer — no network, no live store; `fetch`'s status-code/JSON/tag-shape
/// handling is covered separately below, via its injectable `fetcher`
/// parameter — no stubbed HTTP stack needed, just a canned (data, response)
/// or a thrown error.
final class AppUpdateCheckerTests: XCTestCase {
    func testNewerTagSurfacesVersion() {
        XCTAssertEqual(
            DaemonStore.releaseNewerThanApp(appVersion: "0.6.0", latestTag: "v0.7.0"),
            "0.7.0"
        )
    }

    func testEqualTagIsQuiet() {
        XCTAssertNil(DaemonStore.releaseNewerThanApp(appVersion: "0.6.0", latestTag: "v0.6.0"))
    }

    func testOlderTagIsQuiet() {
        XCTAssertNil(DaemonStore.releaseNewerThanApp(appVersion: "0.7.0", latestTag: "v0.6.0"))
    }

    func testUnstampedAppIsQuiet() {
        // A dev build (0.0.0) must never show a "you're behind" banner.
        XCTAssertNil(DaemonStore.releaseNewerThanApp(appVersion: "0.0.0", latestTag: "v0.7.0"))
    }

    func testMalformedTagIsQuiet() {
        XCTAssertNil(DaemonStore.releaseNewerThanApp(appVersion: "0.6.0", latestTag: "latest"))
        XCTAssertNil(DaemonStore.releaseNewerThanApp(appVersion: "0.6.0", latestTag: ""))
        // A bare "v" with no digits is not a valid tag.
        XCTAssertNil(DaemonStore.releaseNewerThanApp(appVersion: "0.6.0", latestTag: "v"))
    }

    func testBetaSuffixStrippedToCore() {
        // GitHub may publish "v0.7.0-beta.abc123" as a pre-release tag;
        // versionCore anchors at the start and extracts "0.7.0".
        XCTAssertEqual(
            DaemonStore.releaseNewerThanApp(appVersion: "0.6.0", latestTag: "v0.7.0-beta.abc123"),
            "0.7.0"
        )
    }

    func testTagWithoutVPrefixAlsoWorks() {
        // The GitHub API always emits a "v" prefix, but guard the strip.
        XCTAssertEqual(
            DaemonStore.releaseNewerThanApp(appVersion: "0.6.0", latestTag: "0.7.0"),
            "0.7.0"
        )
    }

    func testSemverNumericsCompareProperly() {
        // 0.10.0 > 0.9.0 numerically — the lexical trap.
        XCTAssertEqual(
            DaemonStore.releaseNewerThanApp(appVersion: "0.9.0", latestTag: "v0.10.0"),
            "0.10.0"
        )
        XCTAssertNil(DaemonStore.releaseNewerThanApp(appVersion: "0.10.0", latestTag: "v0.9.0"))
    }

    // MARK: - fetch (status code / JSON / tag shape, via the injectable fetcher)

    private func response(status: Int) -> URLResponse {
        HTTPURLResponse(
            url: URL(string: "https://api.github.com/repos/bojanrajkovic/runny/releases/latest")!,
            statusCode: status, httpVersion: nil, headerFields: nil
        )!
    }

    private func releaseJSON(tag: String, htmlURL: String) -> Data {
        Data("{\"tag_name\":\"\(tag)\",\"html_url\":\"\(htmlURL)\"}".utf8)
    }

    func testFetchSurfacesUpdateOnValidNewerRelease() async {
        let update = await AppUpdateChecker.fetch(appVersion: "0.6.0") { _ in
            (self.releaseJSON(tag: "v0.7.0", htmlURL: "https://github.com/x/releases/v0.7.0"), self.response(status: 200))
        }
        XCTAssertEqual(update?.version, "0.7.0")
        XCTAssertEqual(update?.url, URL(string: "https://github.com/x/releases/v0.7.0"))
    }

    func testFetchIsQuietOnNon200Status() async {
        let update = await AppUpdateChecker.fetch(appVersion: "0.6.0") { _ in
            (self.releaseJSON(tag: "v0.7.0", htmlURL: "https://github.com/x"), self.response(status: 403))
        }
        XCTAssertNil(update, "a rate-limit or error status must never surface a false update")
    }

    func testFetchIsQuietOnMalformedJSON() async {
        let update = await AppUpdateChecker.fetch(appVersion: "0.6.0") { _ in
            (Data("not json".utf8), self.response(status: 200))
        }
        XCTAssertNil(update)
    }

    func testFetchIsQuietOnMalformedHTMLURL() async {
        let update = await AppUpdateChecker.fetch(appVersion: "0.6.0") { _ in
            (self.releaseJSON(tag: "v0.7.0", htmlURL: ""), self.response(status: 200))
        }
        XCTAssertNil(update, "an unparseable html_url must not surface a link that goes nowhere")
    }

    func testFetchIsQuietWhenTagIsNotNewer() async {
        let update = await AppUpdateChecker.fetch(appVersion: "0.7.0") { _ in
            (self.releaseJSON(tag: "v0.6.0", htmlURL: "https://github.com/x"), self.response(status: 200))
        }
        XCTAssertNil(update, "an older or equal tag must stay quiet, same as releaseNewerThanApp alone")
    }

    func testFetchIsQuietWhenTheFetcherThrows() async {
        struct NetworkError: Error {}
        let update = await AppUpdateChecker.fetch(appVersion: "0.6.0") { _ in
            throw NetworkError()
        }
        XCTAssertNil(update, "every network error path is silence, never a false failure banner")
    }
}
