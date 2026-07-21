load("@rules_go//go:def.bzl", "go_library")

go_library(
    name = "cow",
    srcs = ["cow.go"],
    importpath = "github.com/bojanrajkovic/runny/internal/winhcs/cow",
    visibility = ["//internal/winhcs:__subpackages__"],
    deps = select({
        "@rules_go//go/platform:windows": [
            "//internal/winhcs/hcs/schema1",
            "//internal/winhcs/hcs/schema2",
        ],
        "//conditions:default": [],
    }),
)
