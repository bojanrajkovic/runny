"""The Runny app: SwiftUI menu-bar + window client of runnyd over runny.v1.

Builds only on macOS hosts with full Xcode (rules_apple needs the SDK;
Xcode is never opened — ADR-0007). Elsewhere every target is pruned by
target_compatible_with before toolchain resolution.
"""

load("@rules_apple//apple:macos.bzl", "macos_application", "macos_unit_test")
load("@rules_apple//apple:versioning.bzl", "apple_bundle_version")
load("@rules_swift//swift:swift.bzl", "swift_library")
load("//tools/sign:sign.bzl", "app_dmg", "codesign_app", "notarize_app", "notarize_dmg")

swift_library(
    name = "RunnyLib",
    srcs = glob(
        ["Sources/**/*.swift"],
        allow_empty = True,
    ),
    # The menu-bar icon catalog (template image, tinted by the system). The
    # AppIcon catalog is the app bundle's, wired via macos_application.app_icons.
    data = glob(["Resources/Media.xcassets/**"]),
    module_name = "Runny",
    target_compatible_with = ["@platforms//os:macos"],
    deps = ["//proto/runny/v1:runnyv1_swift_proto"],
)

swift_library(
    name = "RunnyTestsLib",
    testonly = True,
    srcs = glob(
        ["Tests/**/*.swift"],
        allow_empty = True,
    ),
    module_name = "RunnyTests",
    target_compatible_with = ["@platforms//os:macos"],
    deps = [":RunnyLib"],
)

# A loadable .xctest bundle, not a swift_test executable: the bundle has no
# main, so RunnyLib's @main entry point doesn't collide with a test runner's.
macos_unit_test(
    name = "RunnyLibTests",
    minimum_os_version = "15.0",
    target_compatible_with = ["@platforms//os:macos"],
    deps = [":RunnyTestsLib"],
)

# CFBundleShortVersionString/CFBundleVersion come from the stamped build
# label (--embed_label="$(tools/version.sh)", ADR-0010). versiontool
# anchors the pattern at the start only (re.match), so "0.5.0" and
# "0.5.0-beta.abc1234" both capture "0.5.0"; unstamped dev builds fall
# back to 0.0.0.
apple_bundle_version(
    name = "version",
    build_label_pattern = "{version}",
    build_version = "{version}",
    capture_groups = {
        "version": "\\d+\\.\\d+\\.\\d+",
    },
    fallback_build_label = "0.0.0",
    short_version_string = "{version}",
    target_compatible_with = ["@platforms//os:macos"],
)

macos_application(
    name = "Runny",
    app_icons = glob(["Resources/AppIcon.xcassets/**"]),
    bundle_id = "com.coderinserepeat.runny",
    infoplists = ["Info.plist"],
    minimum_os_version = "15.0",
    target_compatible_with = ["@platforms//os:macos"],
    version = ":version",
    visibility = ["//visibility:public"],
    deps = [":RunnyLib"],
)

# Distribution chain (see tools/sign/sign.bzl): rules_apple's in-rule
# ad-hoc signature → re-sign → notarize+staple the .app → dmg →
# notarize+staple the dmg. Every notarize step is a pass-through copy at
# the ad-hoc tier.
# The .app carries the same runnyd + runnyctl the brew tarball ships, placed at
# Contents/MacOS/{runnyd,runnyctl} under their bare names and signed inside-out
# (the daemon keeps com.apple.security.virtualization; the CLI gets none, asserted
# at build time). A launchd BundleProgram and the vended-CLI symlink both depend on
# the exact bare names. The binaries are re-sealed here, so they are NOT byte-
# identical to the tarball's _notarized copies (different CDHash) — same source,
# separately sealed and notarized per container.
codesign_app(
    name = "Runny_signed",
    app_zip = ":Runny",
    binaries = {
        "runnyd": "//cmd/runnyd:runnyd_signed",
        "runnyctl": "//cmd/runnyctl:runnyctl_signed",
    },
    entitlement_keys = {"runnyd": "com.apple.security.virtualization"},
    entitlements = {"runnyd": "//tools/sign:runnyd.entitlements"},
    visibility = ["//visibility:public"],
)

notarize_app(
    name = "Runny_notarized_app",
    app_zip = ":Runny_signed",
    visibility = ["//visibility:public"],
)

app_dmg(
    name = "Runny_dmg",
    app_zip = ":Runny_notarized_app",
    dmg_name = "Runny",
    visibility = ["//visibility:public"],
)

notarize_dmg(
    name = "Runny_notarized_dmg",
    dmg = ":Runny_dmg",
    visibility = ["//visibility:public"],
)
