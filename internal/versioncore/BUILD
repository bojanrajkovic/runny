load("@rules_go//go:def.bzl", "go_library", "go_test")

go_library(
    name = "versioncore",
    srcs = ["versioncore.go"],
    importpath = "github.com/bojanrajkovic/runny/internal/versioncore",
    visibility = ["//:__subpackages__"],
)

go_test(
    name = "versioncore_test",
    srcs = ["versioncore_test.go"],
    embed = [":versioncore"],
)
