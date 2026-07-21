load("@rules_go//go:def.bzl", "go_library")

go_library(
    name = "computestorage",
    srcs = [
        "attach.go",
        "destroy.go",
        "detach.go",
        "export.go",
        "format.go",
        "helpers.go",
        "import.go",
        "initialize.go",
        "mount.go",
        "setup.go",
        "storage.go",
        "zsyscall_windows.go",
    ],
    importpath = "github.com/bojanrajkovic/runny/internal/winhcs/computestorage",
    visibility = ["//:__subpackages__"],
    deps = [
        "//internal/winhcs/internal/hcs/schema2",
    ] + select({
        "@rules_go//go/platform:windows": [
            "//internal/winhcs/internal/interop",
            "//internal/winhcs/internal/memory",
            "//internal/winhcs/internal/oc",
            "//internal/winhcs/internal/security",
            "//internal/winhcs/osversion",
            "@com_github_microsoft_go_winio//vhd",
            "@com_github_pkg_errors//:errors",
            "@io_opencensus_go//trace",
            "@org_golang_x_sys//windows",
        ],
        "//conditions:default": [],
    }),
)
