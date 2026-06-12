load("@rules_go//go:def.bzl", "go_library", "go_test")
load("//tools:stamp.bzl", "stamped_go_binary")
load("//tools/sign:sign.bzl", "codesign_binary", "notarize_binary")

# gazelle:map_kind go_binary stamped_go_binary //tools:stamp.bzl

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

stamped_go_binary(
    name = "runnyctl",
    embed = [":runnyctl_lib"],
    visibility = ["//visibility:public"],
)

codesign_binary(
    name = "runnyctl_signed",
    binary = ":runnyctl",
    visibility = ["//visibility:public"],
)

notarize_binary(
    name = "runnyctl_notarized",
    binary = ":runnyctl_signed",
    visibility = ["//visibility:public"],
)

go_test(
    name = "runnyctl_test",
    srcs = ["main_test.go"],
    embed = [":runnyctl_lib"],
    deps = [
        "//proto/runny/v1:runny",
        "@org_golang_google_protobuf//types/known/timestamppb",
    ],
)
