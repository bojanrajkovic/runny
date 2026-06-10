load("@rules_go//go:def.bzl", "go_library")

go_library(
    name = "socket",
    srcs = ["socket.go"],
    importpath = "github.com/bojanrajkovic/runny/internal/socket",
    visibility = ["//:__subpackages__"],
    deps = [
        "//internal/cycle",
        "//internal/logring",
        "//internal/statemachine",
        "//proto/runny/v1:runny",
        "@org_golang_google_grpc//:grpc",
        "@org_golang_google_protobuf//types/known/timestamppb",
    ],
)
