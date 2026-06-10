load("@rules_go//go:def.bzl", "go_library")

go_library(
    name = "images",
    srcs = ["images.go"],
    importpath = "github.com/bojanrajkovic/runny/internal/images",
    visibility = ["//:__subpackages__"],
    deps = [
        "//internal/bounded",
        "//internal/home",
        "//internal/oci",
        "//internal/tart",
    ],
)
