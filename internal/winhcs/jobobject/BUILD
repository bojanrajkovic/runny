load("@rules_go//go:def.bzl", "go_library")

go_library(
    name = "jobobject",
    srcs = [
        "doc.go",
        "iocp.go",
        "jobobject.go",
        "limits.go",
    ],
    importpath = "github.com/bojanrajkovic/runny/internal/winhcs/jobobject",
    visibility = ["//internal/winhcs:__subpackages__"],
    deps = select({
        "@rules_go//go/platform:windows": [
            "//internal/winhcs/log",
            "//internal/winhcs/queue",
            "//internal/winhcs/winapi",
            "@org_golang_x_sys//windows",
        ],
        "//conditions:default": [],
    }),
)
