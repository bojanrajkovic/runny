load("@rules_go//go:def.bzl", "go_library")

go_library(
    name = "osversion",
    srcs = [
        "osversion_windows.go",
        "platform_compat_windows.go",
        "windowsbuilds.go",
    ],
    importpath = "github.com/bojanrajkovic/runny/internal/winhcs/osversion",
    visibility = ["//:__subpackages__"],
    deps = select({
        "@rules_go//go/platform:windows": [
            "@org_golang_x_sys//windows",
            "@org_golang_x_sys//windows/registry",
        ],
        "//conditions:default": [],
    }),
)
