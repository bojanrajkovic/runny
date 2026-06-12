# ADR-0007: No .xcodeproj, ever — headless app packaging via rules_apple

**Status:** Accepted (2026-06-09)

**Amended:** 2026-06-12 — the app (now Runny, ADR-0014) gained a full main
window alongside the menu bar, flipping activation policy dynamically while
it is open, and `Info.plist` is a checked-in file at `apps/Runny/Info.plist`
rather than generated from the BUILD file. The no-.xcodeproj core stands
unchanged.

## Context

The RunnyBar app needs to build into a signed `.app` bundle. The conventional
path is an Xcode project (hand-maintained or generated via Tuist/XcodeGen) and
`xcodebuild`. The owner's constraint: never touch Xcode as an IDE.

## Decision

The app is pure SwiftUI (`MenuBarExtra`, macOS 13+) built headlessly:
`swift_library` → `macos_application` (rules_apple), with Info.plist generated
from the BUILD file (`LSUIElement: true` for menu-bar-only) and
codesigning/entitlements as rule attributes. `rules_xcodeproj` is explicitly
out of scope.

Xcode is an **SDK vendor only**: it must be installed on app-build hosts for
the Swift toolchain and macOS SDK (`xcode-select`), but is never opened.
Notably, the *daemon* side needs less — the 2026-06-09 vz spike compiled and
signed cgo + Virtualization.framework with Command Line Tools alone.

Editor story: SourceKit-LSP in any editor; `sourcekit-bazel-bsp` if
cross-target completion ever matters. SwiftUI live previews are forfeited
knowingly (they only existed inside Xcode).

## Rejected alternatives

- **Tuist-generated project + xcodebuild**: adds a tool whose sole job is
  generating an artifact this decision bans.
- **SPM-only app**: `Package.swift` can produce an executable but not a
  proper signed `.app` bundle with first-class Info.plist control.
- **rules_xcodeproj**: an IDE convenience for an IDE we don't use.
