load("@rules_go//go:def.bzl", "go_library")

go_library(
    name = "vmcompute",
    srcs = [
        "doc.go",
        "vmcompute.go",
        "zsyscall_windows.go",
    ],
    importpath = "github.com/bojanrajkovic/runny/internal/winhcs/vmcompute",
    visibility = ["//internal/winhcs:__subpackages__"],
    deps = select({
        "@rules_go//go/platform:windows": [
            "//internal/winhcs/interop",
            "//internal/winhcs/log",
            "//internal/winhcs/logfields",
            "//internal/winhcs/oc",
            "//internal/winhcs/timeout",
            "@io_opencensus_go//trace",
            "@org_golang_x_sys//windows",
        ],
        "//conditions:default": [],
    }),
)
