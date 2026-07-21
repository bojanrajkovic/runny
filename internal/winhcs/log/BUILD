load("@rules_go//go:def.bzl", "go_library", "go_test")

go_library(
    name = "log",
    srcs = [
        "context.go",
        "scrub.go",
    ],
    importpath = "github.com/bojanrajkovic/runny/internal/winhcs/log",
    visibility = ["//internal/winhcs:__subpackages__"],
    deps = ["//internal/winhcs/hcs/schema2"],
)

go_test(
    name = "log_test",
    srcs = ["context_test.go"],
    embed = [":log"],
)
