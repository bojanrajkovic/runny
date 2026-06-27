import XCTest

@testable import Runny

/// The translocation path heuristic the per-user agent's install eligibility reuses:
/// an `…/AppTranslocation/…` mount or any `/private/var/folders` transient path is
/// translocated (refused, recoverably); a real Applications location is not.
final class TranslocationTests: XCTestCase {
    func testIsTranslocated() {
        XCTAssertTrue(Translocation.isTranslocated("/private/var/folders/ab/cd/T/AppTranslocation/UUID/d/Runny.app"))
        XCTAssertTrue(Translocation.isTranslocated("/private/var/folders/xy/z/Runny.app"))
        XCTAssertFalse(Translocation.isTranslocated("/Applications/Runny.app"))
        XCTAssertFalse(Translocation.isTranslocated("/Users/me/Applications/Runny.app"))
    }
}
