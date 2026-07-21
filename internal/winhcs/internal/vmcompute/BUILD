load("@rules_go//go:def.bzl", "go_library")

go_library(
    name = "vmcompute",
    srcs = [
        "doc.go",
        "vmcompute.go",
        "zsyscall_windows.go",
    ],
    importpath = "github.com/bojanrajkovic/runny/internal/winhcs/internal/vmcompute",
    visibility = ["//internal/winhcs:__subpackages__"],
    deps = select({
        "@rules_go//go/platform:windows": [
            "//internal/winhcs/internal/interop",
            "//internal/winhcs/internal/log",
            "//internal/winhcs/internal/logfields",
            "//internal/winhcs/internal/oc",
            "//internal/winhcs/internal/timeout",
            "@io_opencensus_go//trace",
            "@org_golang_x_sys//windows",
        ],
        "//conditions:default": [],
    }),
)
