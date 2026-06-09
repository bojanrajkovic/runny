load("@rules_go//go:def.bzl", "go_library", "go_test")

go_library(
    name = "cycle",
    srcs = ["cycle.go"],
    importpath = "github.com/bojanrajkovic/runny/internal/cycle",
    visibility = ["//:__subpackages__"],
)

go_test(
    name = "cycle_test",
    srcs = ["cycle_test.go"],
    embed = [":cycle"],
)
