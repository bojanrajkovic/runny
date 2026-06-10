load("@rules_go//go:def.bzl", "go_library", "go_test")

go_library(
    name = "bounded",
    srcs = [
        "bounded.go",
        "stall.go",
    ],
    importpath = "github.com/bojanrajkovic/runny/internal/bounded",
    visibility = ["//:__subpackages__"],
)

go_test(
    name = "bounded_test",
    srcs = ["bounded_test.go"],
    embed = [":bounded"],
)
