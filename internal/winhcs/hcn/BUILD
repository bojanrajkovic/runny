load("@rules_go//go:def.bzl", "go_library")

go_library(
    name = "hcn",
    srcs = [
        "doc.go",
        "hcn.go",
        "hcnendpoint.go",
        "hcnerrors.go",
        "hcnglobals.go",
        "hcnnetwork.go",
        "hcnpolicy.go",
        "hcnsupport.go",
        "zsyscall_windows.go",
    ],
    importpath = "github.com/bojanrajkovic/runny/internal/winhcs/hcn",
    visibility = ["//:__subpackages__"],
    deps = select({
        "@rules_go//go/platform:windows": [
            "//internal/winhcs/internal/hcs",
            "//internal/winhcs/internal/hcserror",
            "//internal/winhcs/internal/interop",
            "//internal/winhcs/internal/log",
            "@com_github_microsoft_go_winio//pkg/guid",
            "@com_github_pkg_errors//:errors",
            "@org_golang_x_sys//windows",
        ],
        "//conditions:default": [],
    }),
)
