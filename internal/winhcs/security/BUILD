load("@rules_go//go:def.bzl", "go_library")

go_library(
    name = "security",
    srcs = [
        "grantvmgroupaccess.go",
        "syscall_windows.go",
        "zsyscall_windows.go",
    ],
    importpath = "github.com/bojanrajkovic/runny/internal/winhcs/security",
    visibility = ["//internal/winhcs:__subpackages__"],
    deps = select({
        "@rules_go//go/platform:windows": [
            "@org_golang_x_sys//windows",
        ],
        "//conditions:default": [],
    }),
)
