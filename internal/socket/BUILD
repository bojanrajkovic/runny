load("@rules_go//go:def.bzl", "go_library", "go_test")

go_library(
    name = "socket",
    srcs = [
        "operator.go",
        "operator_darwin.go",
        "operator_other.go",
        "operator_windows.go",
        "peercred.go",
        "peercred_darwin.go",
        "peercred_other.go",
        "peercred_windows.go",
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
        "@rules_go//go/platform:windows": [
            "@org_golang_x_sys//windows",
        ],
        "//conditions:default": [],
    }),
)

go_test(
    name = "socket_test",
    srcs = [
        "operator_test.go",
        "operator_windows_test.go",
        "peercred_test.go",
        "peercred_windows_test.go",
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
        "@org_golang_google_grpc//metadata",
        "@org_golang_google_grpc//peer",
        "@org_golang_google_grpc//status",
        "@org_golang_google_grpc//test/bufconn",
        "@org_golang_google_protobuf//types/known/durationpb",
        "@org_golang_x_crypto//ssh",
    ] + select({
        "@rules_go//go/platform:darwin": [
            "//internal/bounded",
            "//internal/opacl",
        ],
        "@rules_go//go/platform:ios": [
            "//internal/bounded",
            "//internal/opacl",
        ],
        "@rules_go//go/platform:windows": [
            "@org_golang_x_sys//windows",
        ],
        "//conditions:default": [],
    }),
)
