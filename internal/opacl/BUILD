load("@rules_go//go:def.bzl", "go_library", "go_test")

go_library(
    name = "opacl",
    srcs = [
        "opacl.go",
        "opacl_darwin.go",
        "opacl_other.go",
    ],
    cgo = True,
    importpath = "github.com/bojanrajkovic/runny/internal/opacl",
    visibility = ["//:__subpackages__"],
    deps = select({
        "@rules_go//go/platform:aix": [
            "//internal/bounded",
        ],
        "@rules_go//go/platform:android": [
            "//internal/bounded",
        ],
        "@rules_go//go/platform:darwin": [
            "//internal/bounded",
        ],
        "@rules_go//go/platform:dragonfly": [
            "//internal/bounded",
        ],
        "@rules_go//go/platform:freebsd": [
            "//internal/bounded",
        ],
        "@rules_go//go/platform:illumos": [
            "//internal/bounded",
        ],
        "@rules_go//go/platform:ios": [
            "//internal/bounded",
        ],
        "@rules_go//go/platform:js": [
            "//internal/bounded",
        ],
        "@rules_go//go/platform:linux": [
            "//internal/bounded",
        ],
        "@rules_go//go/platform:netbsd": [
            "//internal/bounded",
        ],
        "@rules_go//go/platform:openbsd": [
            "//internal/bounded",
        ],
        "@rules_go//go/platform:osx": [
            "//internal/bounded",
        ],
        "@rules_go//go/platform:plan9": [
            "//internal/bounded",
        ],
        "@rules_go//go/platform:qnx": [
            "//internal/bounded",
        ],
        "@rules_go//go/platform:solaris": [
            "//internal/bounded",
        ],
        "@rules_go//go/platform:windows": [
            "//internal/bounded",
        ],
        "//conditions:default": [],
    }),
)

go_test(
    name = "opacl_test",
    srcs = [
        "opacl_darwin_test.go",
        "opacl_test.go",
    ],
    embed = [":opacl"],
    deps = select({
        "@rules_go//go/platform:darwin": [
            "//internal/bounded",
        ],
        "@rules_go//go/platform:ios": [
            "//internal/bounded",
        ],
        "//conditions:default": [],
    }),
)
