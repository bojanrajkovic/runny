load("@rules_go//go:def.bzl", "go_library", "go_test")

go_library(
    name = "sysdaemon",
    srcs = [
        "install.go",
        "new_unix.go",
        "new_windows.go",
        "scm_windows.go",
        "scmcmds.go",
        "stage.go",
        "sysdaemon.go",
    ],
    importpath = "github.com/bojanrajkovic/runny/internal/sysdaemon",
    visibility = ["//:__subpackages__"],
    deps = [
        "//internal/home",
        "//internal/launchd",
        "//internal/opacl",
        "@net_howett_plist//:plist",
    ] + select({
        "@rules_go//go/platform:aix": [
            "//internal/testconfig",
        ],
        "@rules_go//go/platform:android": [
            "//internal/testconfig",
        ],
        "@rules_go//go/platform:darwin": [
            "//internal/testconfig",
        ],
        "@rules_go//go/platform:dragonfly": [
            "//internal/testconfig",
        ],
        "@rules_go//go/platform:freebsd": [
            "//internal/testconfig",
        ],
        "@rules_go//go/platform:illumos": [
            "//internal/testconfig",
        ],
        "@rules_go//go/platform:ios": [
            "//internal/testconfig",
        ],
        "@rules_go//go/platform:js": [
            "//internal/testconfig",
        ],
        "@rules_go//go/platform:linux": [
            "//internal/testconfig",
        ],
        "@rules_go//go/platform:netbsd": [
            "//internal/testconfig",
        ],
        "@rules_go//go/platform:openbsd": [
            "//internal/testconfig",
        ],
        "@rules_go//go/platform:osx": [
            "//internal/testconfig",
        ],
        "@rules_go//go/platform:plan9": [
            "//internal/testconfig",
        ],
        "@rules_go//go/platform:qnx": [
            "//internal/testconfig",
        ],
        "@rules_go//go/platform:solaris": [
            "//internal/testconfig",
        ],
        "@rules_go//go/platform:windows": [
            "//internal/testconfig",
            "@org_golang_x_sys//windows",
            "@org_golang_x_sys//windows/svc",
            "@org_golang_x_sys//windows/svc/mgr",
        ],
        "//conditions:default": [],
    }),
)

go_test(
    name = "sysdaemon_test",
    srcs = [
        "scm_windows_test.go",
        "scmcmds_test.go",
        "stage_test.go",
        "sysdaemon_darwin_test.go",
        "sysdaemon_test.go",
    ],
    embed = [":sysdaemon"],
    deps = [
        "//internal/home",
        "//internal/opacl",
    ] + select({
        "@rules_go//go/platform:windows": [
            "@org_golang_x_sys//windows",
            "@org_golang_x_sys//windows/svc",
            "@org_golang_x_sys//windows/svc/mgr",
        ],
        "//conditions:default": [],
    }),
)
