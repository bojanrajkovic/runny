load("@rules_go//go:def.bzl", "go_library", "go_test")

# fixed-min.vhdx is also used by internal/images' own windows-only test
# (a real, valid, minimal VHDX to sniff), rather than duplicating the
# git-lfs-tracked binary fixture in a second package.
exports_files(
    ["testdata/fixed-min.vhdx"],
    visibility = ["//internal/images:__pkg__"],
)

go_library(
    name = "vhdx",
    srcs = [
        "parentlocator.go",
        "reader.go",
        "vhdx.go",
    ],
    importpath = "github.com/bojanrajkovic/runny/internal/vhdx",
    visibility = ["//:__subpackages__"],
    deps = [
        "@com_github_microsoft_go_winio//pkg/guid",
    ] + select({
        "@rules_go//go/platform:windows": [
            "//internal/winhcs/security",
            "@com_github_microsoft_go_winio//vhd",
        ],
        "//conditions:default": [],
    }),
)

go_test(
    name = "vhdx_test",
    srcs = [
        "parentlocator_test.go",
        "reader_test.go",
        "testdata_test.go",
        "vhdx_windows_test.go",
    ],
    data = glob(["testdata/**"]),
    embed = [":vhdx"],
    deps = ["@com_github_microsoft_go_winio//pkg/guid"],
)
