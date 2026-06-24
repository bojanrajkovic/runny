load("@rules_go//go:def.bzl", "go_library", "go_test")

go_library(
    name = "launchd",
    srcs = ["launchd.go"],
    importpath = "github.com/bojanrajkovic/runny/internal/launchd",
    visibility = ["//:__subpackages__"],
)

go_test(
    name = "launchd_test",
    srcs = ["launchd_test.go"],
    embed = [":launchd"],
)
