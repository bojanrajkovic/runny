load("@rules_go//go:def.bzl", "go_binary", "go_library")

go_library(
    name = "runnyd_lib",
    srcs = ["main.go"],
    importpath = "github.com/bojanrajkovic/runny/cmd/runnyd",
    visibility = ["//visibility:private"],
)

go_binary(
    name = "runnyd",
    embed = [":runnyd_lib"],
    visibility = ["//visibility:public"],
)
