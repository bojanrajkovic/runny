load("@rules_go//go:def.bzl", "go_library")

go_library(
    name = "winapi",
    srcs = [
        "bindflt.go",
        "cimfs.go",
        "console.go",
        "devices.go",
        "doc.go",
        "elevation.go",
        "errors.go",
        "filesystem.go",
        "jobobject.go",
        "logon.go",
        "memory.go",
        "net.go",
        "offlinereg.go",
        "path.go",
        "process.go",
        "processor.go",
        "system.go",
        "thread.go",
        "user.go",
        "utils.go",
        "winapi.go",
        "zsyscall_windows.go",
    ],
    importpath = "github.com/bojanrajkovic/runny/internal/winhcs/winapi",
    visibility = ["//internal/winhcs:__subpackages__"],
    deps = select({
        "@rules_go//go/platform:windows": [
            "@com_github_microsoft_go_winio//pkg/guid",
            "@org_golang_x_sys//windows",
        ],
        "//conditions:default": [],
    }),
)
