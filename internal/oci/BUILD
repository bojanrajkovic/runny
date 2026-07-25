load("@rules_go//go:def.bzl", "go_library", "go_test")

go_library(
    name = "oci",
    srcs = [
        "applelz4.go",
        "credentials.go",
        "headroom_other.go",
        "headroom_windows.go",
        "oci.go",
        "pack.go",
    ],
    importpath = "github.com/bojanrajkovic/runny/internal/oci",
    visibility = ["//:__subpackages__"],
    deps = [
        "//internal/bounded",
        "//internal/diskfree",
        "//internal/obs",
        "//internal/tart",
        "@com_github_pierrec_lz4_v4//:lz4",
        "@org_golang_x_sync//errgroup",
    ],
)

go_test(
    name = "oci_test",
    srcs = [
        "applelz4_test.go",
        "credentials_test.go",
        "oci_test.go",
        "pack_test.go",
    ],
    data = glob(["testdata/**"]),
    embed = [":oci"],
    deps = [
        "//internal/bounded",
        "//internal/obs",
        "//internal/tart",
    ],
)
