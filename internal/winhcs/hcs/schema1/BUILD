load("@rules_go//go:def.bzl", "go_library")

go_library(
    name = "schema1",
    srcs = ["schema1.go"],
    importpath = "github.com/bojanrajkovic/runny/internal/winhcs/hcs/schema1",
    visibility = ["//internal/winhcs:__subpackages__"],
    deps = select({
        "@rules_go//go/platform:windows": [
            "//internal/winhcs/hcs/schema2",
            "@com_github_microsoft_go_winio//pkg/guid",
        ],
        "//conditions:default": [],
    }),
)
