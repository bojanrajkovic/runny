load("@rules_go//go:def.bzl", "go_library", "go_test")

go_library(
    name = "images",
    srcs = [
        "disk_darwin.go",
        "disk_other.go",
        "diskformat_other.go",
        "diskformat_windows.go",
        "images.go",
        "prune.go",
        "puller.go",
    ],
    importpath = "github.com/bojanrajkovic/runny/internal/images",
    visibility = ["//:__subpackages__"],
    deps = [
        "//internal/bounded",
        "//internal/diskfree",
        "//internal/home",
        "//internal/obs",
        "//internal/oci",
        "//internal/tart",
        "//internal/versioncore",
    ] + select({
        "@rules_go//go/platform:windows": [
            "//internal/vhdx",
        ],
        "//conditions:default": [],
    }),
)

go_test(
    name = "images_test",
    srcs = [
        "diskformat_windows_test.go",
        "images_test.go",
        "prune_test.go",
        "puller_test.go",
        "tarball_test.go",
    ],
    data = ["//internal/vhdx:testdata/fixed-min.vhdx"],
    embed = [":images"],
    deps = [
        "//internal/bounded",
        "//internal/home",
        "//internal/obs",
        "//internal/oci",
        "//internal/tart",
    ],
)
