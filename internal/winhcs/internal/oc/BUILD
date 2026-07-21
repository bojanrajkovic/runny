load("@rules_go//go:def.bzl", "go_library", "go_test")

go_library(
    name = "oc",
    srcs = [
        "errors.go",
        "span.go",
    ],
    importpath = "github.com/bojanrajkovic/runny/internal/winhcs/internal/oc",
    visibility = ["//internal/winhcs:__subpackages__"],
    deps = [
        "//internal/winhcs/internal/log",
        "@io_opencensus_go//trace",
    ],
)

go_test(
    name = "oc_test",
    srcs = ["errors_test.go"],
    embed = [":oc"],
    deps = ["@io_opencensus_go//trace"],
)
