load("@rules_go//go:def.bzl", "go_library", "go_test")

go_library(
    name = "images",
    srcs = [
        "images.go",
        "puller.go",
    ],
    importpath = "github.com/bojanrajkovic/runny/internal/images",
    visibility = ["//:__subpackages__"],
    deps = [
        "//internal/bounded",
        "//internal/diskfree",
        "//internal/home",
        "//internal/oci",
        "//internal/tart",
        "//internal/versioncore",
    ],
)

go_test(
    name = "images_test",
    srcs = [
        "puller_test.go",
        "tarball_test.go",
    ],
    embed = [":images"],
    deps = ["//internal/oci"],
)
