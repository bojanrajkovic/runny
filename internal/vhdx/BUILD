load("@rules_go//go:def.bzl", "go_library", "go_test")

go_library(
    name = "vhdx",
    srcs = [
        "reader.go",
        "vhdx.go",
    ],
    importpath = "github.com/bojanrajkovic/runny/internal/vhdx",
    visibility = ["//:__subpackages__"],
    deps = [
        "@com_github_microsoft_go_winio//pkg/guid",
    ] + select({
        "@rules_go//go/platform:windows": [
            "@com_github_microsoft_go_winio//vhd",
        ],
        "//conditions:default": [],
    }),
)

go_test(
    name = "vhdx_test",
    srcs = [
        "reader_test.go",
        "testdata_test.go",
        "vhdx_windows_test.go",
    ],
    data = glob(["testdata/**"]),
    embed = [":vhdx"],
    deps = ["@com_github_microsoft_go_winio//pkg/guid"],
)
