load("@rules_go//go:def.bzl", "go_library", "go_test")

go_library(
    name = "tart",
    srcs = [
        "bundle.go",
        "clone.go",
    ],
    importpath = "github.com/bojanrajkovic/runny/internal/tart",
    visibility = ["//:__subpackages__"],
    deps = ["//internal/clonefile"],
)

go_test(
    name = "tart_test",
    srcs = ["bundle_test.go"],
    embed = [":tart"],
)
