load("@rules_go//go:def.bzl", "go_library", "go_test")

go_library(
    name = "socket",
    srcs = [
        "operator.go",
        "peercred.go",
        "peercred_darwin.go",
        "peercred_other.go",
        "registry.go",
        "revocation.go",
        "socket.go",
    ],
    importpath = "github.com/bojanrajkovic/runny/internal/socket",
    visibility = ["//:__subpackages__"],
    deps = [
        "//internal/bounded",
        "//internal/cycle",
        "//internal/home",
        "//internal/logring",
        "//internal/opacl",
        "//internal/statemachine",
        "//internal/versioncore",
        "//proto/runny/v1:runny",
        "@org_golang_google_grpc//:grpc",
        "@org_golang_google_grpc//codes",
        "@org_golang_google_grpc//credentials",
        "@org_golang_google_grpc//credentials/insecure",
        "@org_golang_google_grpc//peer",
        "@org_golang_google_grpc//status",
        "@org_golang_google_protobuf//types/known/durationpb",
        "@org_golang_google_protobuf//types/known/timestamppb",
        "@org_golang_x_crypto//ssh",
    ] + select({
        "@rules_go//go/platform:darwin": [
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:ios": [
            "@org_golang_x_sys//unix",
        ],
        "//conditions:default": [],
    }),
)

go_test(
    name = "socket_test",
    srcs = [
        "operator_test.go",
        "peercred_test.go",
        "registry_test.go",
        "revocation_test.go",
        "socket_test.go",
    ],
    embed = [":socket"],
    deps = [
        "//internal/cycle",
        "//internal/home",
        "//internal/logring",
        "//internal/statemachine",
        "//internal/versioncore",
        "//proto/runny/v1:runny",
        "@org_golang_google_grpc//:grpc",
        "@org_golang_google_grpc//codes",
        "@org_golang_google_grpc//credentials",
        "@org_golang_google_grpc//credentials/insecure",
        "@org_golang_google_grpc//peer",
        "@org_golang_google_grpc//status",
        "@org_golang_google_grpc//test/bufconn",
        "@org_golang_google_protobuf//types/known/durationpb",
        "@org_golang_x_crypto//ssh",
    ] + select({
        "@rules_go//go/platform:darwin": [
            "//internal/bounded",
            "//internal/opacl",
            "@org_golang_google_grpc//metadata",
        ],
        "@rules_go//go/platform:ios": [
            "//internal/bounded",
            "//internal/opacl",
            "@org_golang_google_grpc//metadata",
        ],
        "//conditions:default": [],
    }),
)
