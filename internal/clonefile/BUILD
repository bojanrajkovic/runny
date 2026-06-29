load("@rules_go//go:def.bzl", "go_library", "go_test")

go_library(
    name = "clonefile",
    srcs = [
        "clone_darwin.go",
        "clone_other.go",
    ],
    importpath = "github.com/bojanrajkovic/runny/internal/clonefile",
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
    name = "clonefile_test",
    srcs = ["clone_darwin_test.go"],
    embed = [":clonefile"],
)
