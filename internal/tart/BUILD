load("@rules_go//go:def.bzl", "go_library", "go_test")

go_library(
    name = "tart",
    srcs = [
        "bundle.go",
        "clone_darwin.go",
        "clone_other.go",
    ],
    importpath = "github.com/bojanrajkovic/runny/internal/tart",
    visibility = ["//:__subpackages__"],
    deps = select({
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
    name = "tart_test",
    srcs = ["bundle_test.go"],
    embed = [":tart"],
)
