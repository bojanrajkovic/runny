load("@protobuf//bazel:proto_library.bzl", "proto_library")
load("@rules_go//go:def.bzl", "go_library")
load("@rules_go//proto:def.bzl", "go_proto_library")

proto_library(
    name = "runnyv1_proto",
    srcs = ["runny.proto"],
    visibility = ["//visibility:public"],
    deps = ["@protobuf//:timestamp_proto"],
)

go_proto_library(
    name = "runnyv1_go_proto",
    compilers = [
        "@rules_go//proto:go_proto",
        "@rules_go//proto:go_grpc_v2",
    ],
    importpath = "github.com/bojanrajkovic/runny/proto/runny/v1",
    proto = ":runnyv1_proto",
    visibility = ["//visibility:public"],
)

go_library(
    name = "runny",
    embed = [":runnyv1_go_proto"],
    importpath = "github.com/bojanrajkovic/runny/proto/runny/v1",
    visibility = ["//visibility:public"],
)
