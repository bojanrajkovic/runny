load("@rules_go//go:def.bzl", "go_library", "go_test")

go_library(
    name = "vm",
    srcs = [
        "leases.go",
        "sizing.go",
        "stop.go",
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
        "sizing_test.go",
        "stop_test.go",
    ],
    embed = [":vm"],
    deps = [
        "//internal/bounded",
        "//internal/tart",
    ],
)
