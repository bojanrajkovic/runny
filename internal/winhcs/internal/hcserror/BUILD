load("@rules_go//go:def.bzl", "go_library")

go_library(
    name = "hcserror",
    srcs = [
        "doc.go",
        "hcserror.go",
    ],
    importpath = "github.com/bojanrajkovic/runny/internal/winhcs/internal/hcserror",
    visibility = ["//internal/winhcs:__subpackages__"],
    deps = select({
        "@rules_go//go/platform:windows": [
            "@org_golang_x_sys//windows",
        ],
        "//conditions:default": [],
    }),
)
