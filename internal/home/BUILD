load("@rules_go//go:def.bzl", "go_library", "go_test")

go_library(
    name = "home",
    srcs = [
        "config.go",
        "home.go",
        "home_unix.go",
        "home_windows.go",
        "instance.go",
        "operatorgrants.go",
        "testconfig.go",
        "warnings.go",
    ],
    importpath = "github.com/bojanrajkovic/runny/internal/home",
    visibility = ["//:__subpackages__"],
    deps = [
        "@com_github_goccy_go_yaml//:go-yaml",
    ] + select({
        "@rules_go//go/platform:aix": [
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:android": [
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:darwin": [
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:dragonfly": [
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:freebsd": [
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:illumos": [
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:ios": [
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:js": [
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:linux": [
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:netbsd": [
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:openbsd": [
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:osx": [
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:plan9": [
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:qnx": [
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:solaris": [
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:windows": [
            "@org_golang_x_sys//windows",
        ],
        "//conditions:default": [],
    }),
)

go_test(
    name = "home_test",
    srcs = [
        "config_test.go",
        "instance_test.go",
        "instance_unix_test.go",
        "operatorgrants_test.go",
        "resolve_test.go",
        "socketpath_unix_test.go",
        "socketpath_windows_test.go",
        "testconfig_test.go",
        "warnings_test.go",
    ],
    embed = [":home"],
)
