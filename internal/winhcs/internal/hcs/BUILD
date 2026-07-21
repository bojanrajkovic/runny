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
    importpath = "github.com/bojanrajkovic/runny/internal/winhcs/internal/hcs",
    visibility = ["//internal/winhcs:__subpackages__"],
    deps = select({
        "@rules_go//go/platform:windows": [
            "//internal/winhcs/computestorage",
            "//internal/winhcs/internal/cow",
            "//internal/winhcs/internal/hcs/schema1",
            "//internal/winhcs/internal/hcs/schema2",
            "//internal/winhcs/internal/interop",
            "//internal/winhcs/internal/jobobject",
            "//internal/winhcs/internal/log",
            "//internal/winhcs/internal/logfields",
            "//internal/winhcs/internal/oc",
            "//internal/winhcs/internal/protocol/guestrequest",
            "//internal/winhcs/internal/timeout",
            "//internal/winhcs/internal/vmcompute",
            "@com_github_microsoft_go_winio//:go-winio",
            "@com_github_microsoft_go_winio//vhd",
            "@com_github_pkg_errors//:errors",
            "@io_opencensus_go//trace",
            "@org_golang_x_sys//windows",
        ],
        "//conditions:default": [],
    }),
)
