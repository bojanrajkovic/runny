"""Formatter binary, in its own package to avoid eager loading for non-format builds."""

load("@aspect_rules_lint//format:defs.bzl", "format_multirun")

package(default_visibility = ["//:__subpackages__"])

format_multirun(
    name = "format",
    go = "@aspect_rules_lint//format:gofumpt",
    starlark = "@buildifier_prebuilt//:buildifier",
)
