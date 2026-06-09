"""nogo static analysis, wired into the Go toolchain via MODULE.bazel."""

load("@rules_go//go:def.bzl", "TOOLS_NOGO", "nogo")

package(default_visibility = ["//visibility:public"])

nogo(
    name = "nogo",
    visibility = ["//visibility:public"],
    deps = TOOLS_NOGO,
)
