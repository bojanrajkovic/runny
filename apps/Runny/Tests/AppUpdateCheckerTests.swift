import XCTest

@testable import Runny

/// The app-update notify path: `releaseNewerThanApp` is the pure decision
/// layer — no network, no live store. The fetch layer is intentionally thin
/// (it wraps URLSession and calls this function); its correctness is the
/// only networked path and is verified by the manual round-trip, not by tests
/// that would require a stubbed HTTP stack.
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
}
