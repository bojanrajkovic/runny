load("@rules_go//go:def.bzl", "go_library", "go_test")

go_library(
    name = "oci",
    srcs = [
        "applelz4.go",
        "oci.go",
    ],
    importpath = "github.com/bojanrajkovic/runny/internal/oci",
    visibility = ["//:__subpackages__"],
    deps = [
        "@com_github_pierrec_lz4_v4//:lz4",
        "@org_golang_x_sync//errgroup",
    ],
)

go_test(
    name = "oci_test",
    srcs = ["oci_test.go"],
    embed = [":oci"],
    deps = ["@com_github_pierrec_lz4_v4//:lz4"],
)
