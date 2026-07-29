load("@rules_go//go:def.bzl", "go_library", "go_test")
load("//tools:stamp.bzl", "stamped_go_binary")
load("//tools/sign:sign.bzl", "codesign_binary", "notarize_binary")

# gazelle:map_kind go_binary stamped_go_binary //tools:stamp.bzl

go_library(
    name = "runnyctl_lib",
    srcs = [
        "cli.go",
        "dial_unix.go",
        "dial_windows.go",
        "editconfig.go",
        "execssh_unix.go",
        "execssh_windows.go",
        "follow.go",
        "image.go",
        "installdaemon.go",
        "installdaemon_unix.go",
        "installdaemon_windows.go",
        "main.go",
        "operator.go",
        "skew.go",
        "upgradedaemon.go",
    ],
    importpath = "github.com/bojanrajkovic/runny/cmd/runnyctl",
    visibility = ["//visibility:private"],
    deps = [
        "//internal/cycle",
        "//internal/home",
        "//internal/launchd",
        "//internal/oci",
        "//internal/sysdaemon",
        "//internal/tart",
        "//internal/testconfig",
        "//internal/versioncore",
        "//proto/runny/v1:runny",
        "@com_github_alecthomas_kong//:kong",
        "@com_github_rivo_uniseg//:uniseg",
        "@org_golang_google_grpc//codes",
        "@org_golang_google_grpc//connectivity",
        "@org_golang_google_grpc//status",
        "@org_golang_google_protobuf//encoding/protojson",
        "@org_golang_google_protobuf//proto",
        "@org_golang_google_protobuf//types/known/durationpb",
        "@org_golang_x_term//:term",
    ] + select({
        "@rules_go//go/platform:aix": [
            "@org_golang_google_grpc//:grpc",
            "@org_golang_google_grpc//credentials/insecure",
        ],
        "@rules_go//go/platform:android": [
            "@org_golang_google_grpc//:grpc",
            "@org_golang_google_grpc//credentials/insecure",
        ],
        "@rules_go//go/platform:darwin": [
            "@org_golang_google_grpc//:grpc",
            "@org_golang_google_grpc//credentials/insecure",
        ],
        "@rules_go//go/platform:dragonfly": [
            "@org_golang_google_grpc//:grpc",
            "@org_golang_google_grpc//credentials/insecure",
        ],
        "@rules_go//go/platform:freebsd": [
            "@org_golang_google_grpc//:grpc",
            "@org_golang_google_grpc//credentials/insecure",
        ],
        "@rules_go//go/platform:illumos": [
            "@org_golang_google_grpc//:grpc",
            "@org_golang_google_grpc//credentials/insecure",
        ],
        "@rules_go//go/platform:ios": [
            "@org_golang_google_grpc//:grpc",
            "@org_golang_google_grpc//credentials/insecure",
        ],
        "@rules_go//go/platform:js": [
            "@org_golang_google_grpc//:grpc",
            "@org_golang_google_grpc//credentials/insecure",
        ],
        "@rules_go//go/platform:linux": [
            "@org_golang_google_grpc//:grpc",
            "@org_golang_google_grpc//credentials/insecure",
        ],
        "@rules_go//go/platform:netbsd": [
            "@org_golang_google_grpc//:grpc",
            "@org_golang_google_grpc//credentials/insecure",
        ],
        "@rules_go//go/platform:openbsd": [
            "@org_golang_google_grpc//:grpc",
            "@org_golang_google_grpc//credentials/insecure",
        ],
        "@rules_go//go/platform:osx": [
            "@org_golang_google_grpc//:grpc",
            "@org_golang_google_grpc//credentials/insecure",
        ],
        "@rules_go//go/platform:plan9": [
            "@org_golang_google_grpc//:grpc",
            "@org_golang_google_grpc//credentials/insecure",
        ],
        "@rules_go//go/platform:qnx": [
            "@org_golang_google_grpc//:grpc",
            "@org_golang_google_grpc//credentials/insecure",
        ],
        "@rules_go//go/platform:solaris": [
            "@org_golang_google_grpc//:grpc",
            "@org_golang_google_grpc//credentials/insecure",
        ],
        "@rules_go//go/platform:windows": [
            "@com_github_microsoft_go_winio//:go-winio",
            "@org_golang_google_grpc//:grpc",
            "@org_golang_google_grpc//credentials/insecure",
            "@org_golang_x_sys//windows",
        ],
        "//conditions:default": [],
    }),
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
    srcs = [
        "dial_windows_test.go",
        "editconfig_test.go",
        "follow_test.go",
        "image_test.go",
        "installdaemon_test.go",
        "installdaemon_windows_test.go",
        "main_test.go",
        "operator_test.go",
        "skew_test.go",
        "upgradedaemon_test.go",
    ],
    embed = [":runnyctl_lib"],
    deps = [
        "//internal/cycle",
        "//internal/home",
        "//internal/launchd",
        "//proto/runny/v1:runny",
        "@org_golang_google_grpc//:grpc",
        "@org_golang_google_grpc//codes",
        "@org_golang_google_grpc//status",
        "@org_golang_google_protobuf//types/known/timestamppb",
    ] + select({
        "@rules_go//go/platform:windows": [
            "@com_github_microsoft_go_winio//:go-winio",
            "@org_golang_x_sys//windows",
        ],
        "//conditions:default": [],
    }),
)

stamped_go_binary(
    name = "runnyctl",
    embed = [":runnyctl_lib"],
    visibility = ["//visibility:public"],
)
