load("@rules_go//go:def.bzl", "go_binary", "go_library")

go_library(
    name = "runnyd_lib",
    srcs = [
        "main.go",
        "platform_darwin.go",
        "platform_other.go",
    ],
    importpath = "github.com/bojanrajkovic/runny/cmd/runnyd",
    visibility = ["//visibility:private"],
    deps = [
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
    ] + select({
        "@rules_go//go/platform:aix": [
            "//internal/vm",
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:android": [
            "//internal/vm",
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:darwin": [
            "//internal/vm",
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:dragonfly": [
            "//internal/vm",
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:freebsd": [
            "//internal/vm",
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:illumos": [
            "//internal/vm",
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:ios": [
            "//internal/vm",
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:js": [
            "//internal/vm",
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:linux": [
            "//internal/vm",
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:netbsd": [
            "//internal/vm",
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:openbsd": [
            "//internal/vm",
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:osx": [
            "//internal/vm",
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:plan9": [
            "//internal/vm",
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:qnx": [
            "//internal/vm",
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:solaris": [
            "//internal/vm",
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:windows": [
            "//internal/vm",
            "@org_golang_x_sys//unix",
        ],
        "//conditions:default": [],
    }),
)

go_binary(
    name = "runnyd",
    embed = [":runnyd_lib"],
    visibility = ["//visibility:public"],
)
