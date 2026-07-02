load("@rules_go//go:def.bzl", "go_library", "go_test")

go_library(
    name = "traceid",
    srcs = ["traceid.go"],
    importpath = "github.com/bojanrajkovic/runny/internal/traceid",
    visibility = ["//:__subpackages__"],
)

go_test(
    name = "traceid_test",
    srcs = ["traceid_test.go"],
    embed = [":traceid"],
)
