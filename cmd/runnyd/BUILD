load("@rules_go//go:def.bzl", "go_library", "go_test")
load("//tools:stamp.bzl", "stamped_go_binary")

# gazelle:map_kind go_binary stamped_go_binary //tools:stamp.bzl

go_library(
    name = "runnyd_lib",
    srcs = [
        "lock.go",
        "main.go",
        "platform_darwin.go",
        "platform_other.go",
        "rotate.go",
    ],
    importpath = "github.com/bojanrajkovic/runny/cmd/runnyd",
    visibility = ["//visibility:private"],
    deps = [
        "//internal/bounded",
        "//internal/cycle",
        "//internal/github",
        "//internal/guest",
        "//internal/home",
        "//internal/images",
        "//internal/logring",
        "//internal/oci",
        "//internal/socket",
        "//internal/sshx",
        "//internal/statemachine",
        "//internal/tart",
        "@org_golang_x_sys//unix",
    ] + select({
        "@rules_go//go/platform:aix": [
            "//internal/vm",
        ],
        "@rules_go//go/platform:android": [
            "//internal/vm",
        ],
        "@rules_go//go/platform:darwin": [
            "//internal/vm",
        ],
        "@rules_go//go/platform:dragonfly": [
            "//internal/vm",
        ],
        "@rules_go//go/platform:freebsd": [
            "//internal/vm",
        ],
        "@rules_go//go/platform:illumos": [
            "//internal/vm",
        ],
        "@rules_go//go/platform:ios": [
            "//internal/vm",
        ],
        "@rules_go//go/platform:js": [
            "//internal/vm",
        ],
        "@rules_go//go/platform:linux": [
            "//internal/vm",
        ],
        "@rules_go//go/platform:netbsd": [
            "//internal/vm",
        ],
        "@rules_go//go/platform:openbsd": [
            "//internal/vm",
        ],
        "@rules_go//go/platform:osx": [
            "//internal/vm",
        ],
        "@rules_go//go/platform:plan9": [
            "//internal/vm",
        ],
        "@rules_go//go/platform:qnx": [
            "//internal/vm",
        ],
        "@rules_go//go/platform:solaris": [
            "//internal/vm",
        ],
        "@rules_go//go/platform:windows": [
            "//internal/vm",
        ],
        "//conditions:default": [],
    }),
)

stamped_go_binary(
    name = "runnyd",
    embed = [":runnyd_lib"],
    visibility = ["//visibility:public"],
)

go_test(
    name = "runnyd_test",
    srcs = ["rotate_test.go"],
    embed = [":runnyd_lib"],
)
