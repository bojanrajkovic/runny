load("@rules_go//go:def.bzl", "go_library")

go_library(
    name = "hcs",
    srcs = [
        "callback.go",
        "doc.go",
        "errors.go",
        "process.go",
        "service.go",
        "system.go",
        "utils.go",
        "waithelper.go",
    ],
    importpath = "github.com/bojanrajkovic/runny/internal/winhcs/hcs",
    visibility = ["//internal/winhcs:__subpackages__"],
    deps = select({
        "@rules_go//go/platform:windows": [
            "//internal/winhcs/computestorage",
            "//internal/winhcs/cow",
            "//internal/winhcs/hcs/schema1",
            "//internal/winhcs/hcs/schema2",
            "//internal/winhcs/interop",
            "//internal/winhcs/jobobject",
            "//internal/winhcs/log",
            "//internal/winhcs/logfields",
            "//internal/winhcs/oc",
            "//internal/winhcs/protocol/guestrequest",
            "//internal/winhcs/timeout",
            "//internal/winhcs/vmcompute",
            "@com_github_microsoft_go_winio//:go-winio",
            "@com_github_microsoft_go_winio//vhd",
            "@com_github_pkg_errors//:errors",
            "@io_opencensus_go//trace",
            "@org_golang_x_sys//windows",
        ],
        "//conditions:default": [],
    }),
)
