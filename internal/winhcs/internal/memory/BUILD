load("@rules_go//go:def.bzl", "go_library")

go_library(
    name = "memory",
    srcs = [
        "pool.go",
        "types.go",
    ],
    importpath = "github.com/bojanrajkovic/runny/internal/winhcs/internal/memory",
    visibility = ["//internal/winhcs:__subpackages__"],
    deps = ["@com_github_pkg_errors//:errors"],
)
