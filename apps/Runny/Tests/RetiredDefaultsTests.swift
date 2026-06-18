import XCTest

@testable import Runny

/// When a Settings field is removed, the UserDefaults key it wrote lingers in
/// the user's defaults plist — inert, but orphaned state. The fixed-home change
/// retired the "Runny home" override; `RetiredDefaults.prune` drops the dead
/// key once at launch. It is idempotent (removing an absent key is a no-op), so
/// it can run unconditionally without a "have I migrated?" flag, and it must
/// never touch a live key.
final class RetiredDefaultsTests: XCTestCase {
    private func isolatedDefaults() -> (UserDefaults, String) {
        let suite = "RetiredDefaultsTests-\(UUID().uuidString)"
        let defaults = UserDefaults(suiteName: suite)!
        // A previously-crashed run could have left state in this suite name —
        // start from a clean domain so each case is independent.
        defaults.removePersistentDomain(forName: suite)
        return (defaults, suite)
    }

    func testPruneRemovesRetiredHomeOverrideKey() {
        let (defaults, suite) = isolatedDefaults()
        defer { defaults.removePersistentDomain(forName: suite) }
        defaults.set("/tmp/junk-home", forKey: "runnyHomeOverride")
        XCTAssertNotNil(defaults.object(forKey: "runnyHomeOverride"))

        RetiredDefaults.prune(defaults)

        XCTAssertNil(
            defaults.object(forKey: "runnyHomeOverride"),
            "the retired home-override key must be removed"
        )
    }

    func testPruneIsAHarmlessNoOpWhenAbsent() {
        let (defaults, suite) = isolatedDefaults()
        defer { defaults.removePersistentDomain(forName: suite) }
        // Nothing set; pruning must not crash or fault anything in.
        RetiredDefaults.prune(defaults)
        XCTAssertNil(defaults.object(forKey: "runnyHomeOverride"))
    }

    func testPrunePreservesLiveKeys() {
        let (defaults, suite) = isolatedDefaults()
        defer { defaults.removePersistentDomain(forName: suite) }
        defaults.set("keep-me", forKey: "someLiveKey")

        RetiredDefaults.prune(defaults)

        XCTAssertEqual(
            defaults.string(forKey: "someLiveKey"), "keep-me",
            "pruning retired keys must not touch a live one"
        )
    }
}
