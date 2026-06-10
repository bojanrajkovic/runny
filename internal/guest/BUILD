load("@rules_go//go:def.bzl", "go_library")

go_library(
    name = "guest",
    srcs = ["guest.go"],
    importpath = "github.com/bojanrajkovic/runny/internal/guest",
    visibility = ["//:__subpackages__"],
    deps = [
        "//internal/bounded",
        "//internal/sshx",
        "//internal/statemachine",
    ],
)
