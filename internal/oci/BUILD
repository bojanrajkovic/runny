load("@rules_go//go:def.bzl", "go_library", "go_test")

go_library(
    name = "oci",
    srcs = [
        "applelz4.go",
        "oci.go",
        "statfs.go",
    ],
    importpath = "github.com/bojanrajkovic/runny/internal/oci",
    visibility = ["//:__subpackages__"],
    deps = [
        "//internal/bounded",
        "//internal/tart",
        "@com_github_pierrec_lz4_v4//:lz4",
        "@org_golang_x_sync//errgroup",
    ] + select({
        "@rules_go//go/platform:android": [
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:darwin": [
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:ios": [
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:linux": [
            "@org_golang_x_sys//unix",
        ],
        "//conditions:default": [],
    }),
)

go_test(
    name = "oci_test",
    srcs = [
        "applelz4_test.go",
        "oci_test.go",
    ],
    data = glob(["testdata/**"]),
    embed = [":oci"],
    deps = [
        "//internal/bounded",
        "@com_github_pierrec_lz4_v4//:lz4",
    ],
)
