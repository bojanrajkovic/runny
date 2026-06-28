load("@rules_go//go:def.bzl", "go_library", "go_test")

go_library(
    name = "respawn",
    srcs = ["respawn.go"],
    importpath = "github.com/bojanrajkovic/runny/internal/respawn",
    visibility = ["//:__subpackages__"],
    deps = [
        "//internal/home",
        "//internal/sysdaemon",
        "@net_howett_plist//:plist",
    ],
)

go_test(
    name = "respawn_test",
    srcs = ["respawn_test.go"],
    embed = [":respawn"],
    deps = [
        "//internal/home",
        "@net_howett_plist//:plist",
    ],
)
