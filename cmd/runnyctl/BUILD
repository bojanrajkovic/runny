load("@rules_go//go:def.bzl", "go_binary", "go_library")

go_library(
    name = "runnyctl_lib",
    srcs = ["main.go"],
    importpath = "github.com/bojanrajkovic/runny/cmd/runnyctl",
    visibility = ["//visibility:private"],
    deps = ["//proto/runny/v1:runny"],
)

go_binary(
    name = "runnyctl",
    embed = [":runnyctl_lib"],
    visibility = ["//visibility:public"],
)
