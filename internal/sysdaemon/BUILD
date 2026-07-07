load("@rules_go//go:def.bzl", "go_library", "go_test")

go_library(
    name = "sysdaemon",
    srcs = [
        "install.go",
        "stage.go",
        "sysdaemon.go",
    ],
    importpath = "github.com/bojanrajkovic/runny/internal/sysdaemon",
    visibility = ["//:__subpackages__"],
    deps = [
        "//internal/home",
        "//internal/launchd",
        "//internal/opacl",
        "//internal/testconfig",
        "@net_howett_plist//:plist",
    ],
)

go_test(
    name = "sysdaemon_test",
    srcs = [
        "stage_test.go",
        "sysdaemon_test.go",
    ],
    embed = [":sysdaemon"],
    deps = [
        "//internal/home",
        "//internal/opacl",
    ],
)
