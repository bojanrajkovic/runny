load("@rules_go//go:def.bzl", "go_binary", "go_library")

go_library(
    name = "seedpull_lib",
    srcs = ["main.go"],
    importpath = "github.com/bojanrajkovic/runny/tools/seedpull",
    visibility = ["//visibility:private"],
    deps = [
        "//internal/bounded",
        "//internal/home",
        "//internal/oci",
    ],
)

go_binary(
    name = "seedpull",
    embed = [":seedpull_lib"],
    visibility = ["//visibility:public"],
)
