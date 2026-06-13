"""Formatter binary, in its own package to avoid eager loading for non-format builds."""

load("@aspect_rules_lint//format:defs.bzl", "format_multirun")

package(default_visibility = ["//:__subpackages__"])

# SwiftFormat (nicklockwood) prebuilt binaries from rules_lint's format
# tools extension (see MODULE.bazel); config lives in /.swiftformat.
alias(
    name = "swiftformat",
    actual = select({
        "@platforms//os:macos": "@swiftformat_mac//:swiftformat_mac",
        "//conditions:default": "@swiftformat//:swiftformat",
    }),
)

format_multirun(
    name = "format",
    go = "@aspect_rules_lint//format:gofumpt",
    starlark = "@buildifier_prebuilt//:buildifier",
    swift = ":swiftformat",
)
