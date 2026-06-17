import XCTest

@testable import Runny

import RunnyV1

/// The version-skew taxonomy: given the app's stamped facts and the daemon's
/// live `version`/`protocol_version`, classify the skew — warn, never refuse.
/// The load-bearing correction is normalization: the daemon publishes a
/// suffix-bearing label (`0.6.0-beta.<sha>`) while the app stamps the bare
/// `0.6.0`, so a same-commit beta pair must compare equal and stay quiet. Pure,
/// so every branch is pinned without a live daemon.
final class SkewVerdictTests: XCTestCase {
    private func verdict(
        app: String = "0.6.0", appProto: UInt32 = 2,
        daemon: String = "0.6.0", daemonProto: UInt32 = 2
    ) -> DaemonStore.SkewVerdict? {
        DaemonStore.skewVerdict(
            appVersion: app, appExpectedProtocol: appProto,
            daemonVersion: daemon, daemonProtocol: daemonProto
        )
    }

    func testBetaPairSameCommitIsQuiet() {
        // The common case: at a beta release the SAME commit ships an app stamped
        // 0.6.0 and a daemon reporting 0.6.0-beta.<sha>. Comparing raw strings
        // would false-alarm on every CI/dev/pre-release install — the worst silent
        // failure (alarm fatigue). Normalized cores match, so it stays quiet.
        XCTAssertNil(verdict(daemon: "0.6.0-beta.abc12345"))
    }

    func testVersionCoreExtraction() {
        XCTAssertEqual(DaemonStore.versionCore("0.6.0-beta.abc12345"), "0.6.0")
        XCTAssertEqual(DaemonStore.versionCore("0.6.0"), "0.6.0")
        XCTAssertEqual(DaemonStore.versionCore("12.34.56-rc.1"), "12.34.56")
        XCTAssertNil(DaemonStore.versionCore(""))
        XCTAssertNil(DaemonStore.versionCore("dev"))
        // Anchored at the start (mirroring the build's re.match): a triple that
        // isn't the leading token must not be mis-extracted from the middle, so a
        // non-conforming label fails safe to nil → quiet rather than guessing.
        XCTAssertNil(DaemonStore.versionCore("v0.6.0"))
        XCTAssertNil(DaemonStore.versionCore("ci-2024.01.15-0.6.0"))
    }

    func testEmptyDaemonVersionIsQuiet() {
        // Fresh connect, or a daemon predating the version field: no version heard
        // yet, so never warn — no flap at connect.
        XCTAssertNil(verdict(daemon: ""))
    }

    func testUnstampedDevAppIsQuiet() {
        // A dev build (unstamped → 0.0.0, the build's fallback) must not wear a
        // permanent false banner against any real daemon.
        XCTAssertNil(verdict(app: "0.0.0", daemon: "0.6.0"))
    }

    func testDifferentReleaseWarns() {
        // The shared-host case: a brew-managed daemon at a wholly different x.y.z.
        let v = verdict(app: "0.6.0", daemon: "0.5.0")
        XCTAssertEqual(v?.kind, .versionMismatch)
        XCTAssertTrue(v?.text.contains("0.6.0") ?? false, "names the app core")
        XCTAssertTrue(v?.text.contains("0.5.0") ?? false, "names the daemon version")
    }

    func testVersionMismatchTextUsesStableCoreNotVolatileSuffix() {
        // The text names the daemon's CORE, not its full sha-bearing string, so a
        // same-core daemon rebuild that only rotates the build sha produces an
        // identical verdict — a dismissed banner stays dismissed instead of
        // re-popping on cosmetic churn. The full daemon version is shown in the
        // version line above the banner, so nothing is lost.
        let v1 = verdict(app: "0.6.0", daemon: "0.5.0-beta.deadbeef")
        let v2 = verdict(app: "0.6.0", daemon: "0.5.0-beta.cafef00d")
        XCTAssertEqual(v1?.kind, .versionMismatch)
        XCTAssertTrue(v1?.text.contains("0.5.0") ?? false, "names the daemon core")
        XCTAssertFalse(v1?.text.contains("deadbeef") ?? true, "excludes the volatile suffix")
        XCTAssertEqual(v1, v2, "a same-core sha rotation must yield an identical verdict")
    }

    func testUpgradeWindowProtocolBehindWarns() {
        // The canonical upgrade window: new app, old daemon at the SAME x.y.z but
        // a lower protocol. The version axis is blind here (cores match); only the
        // protocol axis sees it. This is the primary upgrade-window detector.
        let v = verdict(app: "0.6.0", appProto: 2, daemon: "0.6.0", daemonProto: 1)
        XCTAssertEqual(v?.kind, .protocolBehind)
        XCTAssertTrue(v?.text.contains("upgrade or restart runnyd") ?? false)
    }

    func testNewerDaemonProtocolIsQuiet() {
        // Lockstep drift the safe way: a daemon ahead of the app (protocol 3 vs an
        // app expecting 2) degrades nothing — the monotone direction. Pins that
        // branch uses `<`, not `!=`, so an old-app/new-daemon pair never warns.
        XCTAssertNil(verdict(app: "0.6.0", appProto: 2, daemon: "0.6.0", daemonProto: 3))
    }

    func testMatchedVersionAndProtocolIsQuiet() {
        XCTAssertNil(verdict(app: "0.6.0", appProto: 2, daemon: "0.6.0", daemonProto: 2))
    }
}

/// The two visibility gates on a computed skew, both silent-failure defenses:
/// the connection gate (never assert skew about a daemon that may have recycled)
/// and the dismiss gate (suppress what the operator dismissed, but re-surface a
/// changed verdict). Pure, so pinned without a live store — the gate lives in one
/// place so no view can re-implement, and forget, it.
final class SkewVisibilityGateTests: XCTestCase {
    private let a = DaemonStore.SkewVerdict(kind: .versionMismatch, text: "app 0.6.0 vs daemon 0.5.0")
    private let b = DaemonStore.SkewVerdict(kind: .protocolBehind, text: "daemon predates a capability")

    func testShownWhenConnectedAndNotDismissed() {
        XCTAssertEqual(
            DaemonStore.gatedSkew(skew: a, connection: .connected, dismissed: nil), a
        )
    }

    func testHiddenWhenNotConnected() {
        // A stored skew must not survive a drop: the supervisor flips `connection`
        // without re-running apply(), so the gate is the only thing keeping a
        // gone/recycled daemon from being lied about.
        for state in [
            DaemonStore.ConnectionState.connecting,
            .reconnecting,
            .stale(since: Date()),
            .unreachable(reason: "no socket"),
        ] {
            XCTAssertNil(
                DaemonStore.gatedSkew(skew: a, connection: state, dismissed: nil),
                "skew must be hidden while \(state)"
            )
        }
    }

    func testDismissedSkewIsSuppressed() {
        XCTAssertNil(DaemonStore.gatedSkew(skew: a, connection: .connected, dismissed: a))
    }

    func testWorseningSkewResurfacesPastDismissal() {
        // Dismiss verdict A, then a DIFFERENT verdict B arrives (different axis, or
        // a worsening on the same version string). Keying dismissal on the version
        // string would hide it; keying on the Equatable value re-surfaces it.
        XCTAssertEqual(
            DaemonStore.gatedSkew(skew: b, connection: .connected, dismissed: a), b
        )
    }

    func testNoSkewShowsNothing() {
        XCTAssertNil(DaemonStore.gatedSkew(skew: nil, connection: .connected, dismissed: nil))
    }
}
