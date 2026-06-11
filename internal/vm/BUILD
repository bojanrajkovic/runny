load("@rules_go//go:def.bzl", "go_library", "go_test")

go_library(
    name = "vm",
    srcs = [
        "leases.go",
        "vm.go",
        "vz_darwin.go",
    ],
    importpath = "github.com/bojanrajkovic/runny/internal/vm",
    visibility = ["//:__subpackages__"],
    deps = [
        "//internal/bounded",
        "//internal/tart",
    ] + select({
        "@rules_go//go/platform:darwin": [
            "@com_github_code_hex_vz_v3//:vz",
        ],
        "@rules_go//go/platform:ios": [
            "@com_github_code_hex_vz_v3//:vz",
        ],
        "//conditions:default": [],
    }),
)

go_test(
    name = "vm_test",
    srcs = [
        "leases_test.go",
        "vz_sizing_test.go",
    ],
    embed = [":vm"],
    deps = select({
        "@rules_go//go/platform:darwin": [
            "//internal/tart",
        ],
        "@rules_go//go/platform:ios": [
            "//internal/tart",
        ],
        "//conditions:default": [],
    }),
)
