load("@rules_go//go:def.bzl", "go_binary", "go_library")

go_library(
    name = "runnyctl_lib",
    srcs = ["main.go"],
    importpath = "github.com/bojanrajkovic/runny/cmd/runnyctl",
    visibility = ["//visibility:private"],
    deps = [
        "//internal/home",
        "//proto/runny/v1:runny",
        "@org_golang_google_grpc//:grpc",
        "@org_golang_google_grpc//credentials/insecure",
        "@org_golang_google_protobuf//encoding/protojson",
        "@org_golang_google_protobuf//proto",
    ],
)

go_binary(
    name = "runnyctl",
    embed = [":runnyctl_lib"],
    visibility = ["//visibility:public"],
    # Stamped under --config=release (ADR-0010); "dev" otherwise.
    x_defs = {"main.version": "{STABLE_VERSION}"},
)
