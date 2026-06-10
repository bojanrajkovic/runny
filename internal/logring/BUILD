load("@rules_go//go:def.bzl", "go_library", "go_test")

go_library(
    name = "logring",
    srcs = ["logring.go"],
    importpath = "github.com/bojanrajkovic/runny/internal/logring",
    visibility = ["//:__subpackages__"],
)

go_test(
    name = "logring_test",
    srcs = ["logring_test.go"],
    embed = [":logring"],
)
