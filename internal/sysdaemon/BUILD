load("@rules_go//go:def.bzl", "go_library", "go_test")

go_library(
    name = "sysdaemon",
    srcs = [
        "install.go",
        "sysdaemon.go",
    ],
    importpath = "github.com/bojanrajkovic/runny/internal/sysdaemon",
    visibility = ["//:__subpackages__"],
    deps = ["//internal/home"],
)

go_test(
    name = "sysdaemon_test",
    srcs = ["sysdaemon_test.go"],
    embed = [":sysdaemon"],
    deps = ["//internal/home"],
)
