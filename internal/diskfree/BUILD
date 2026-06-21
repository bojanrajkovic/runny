load("@rules_go//go:def.bzl", "go_library")

go_library(
    name = "diskfree",
    srcs = [
        "diskfree_darwin.go",
        "diskfree_linux.go",
    ],
    cgo = True,
    clinkopts = select({
        "@rules_go//go/platform:darwin": [
            "-framework CoreFoundation",
        ],
        "//conditions:default": [],
    }),
    importpath = "github.com/bojanrajkovic/runny/internal/diskfree",
    visibility = ["//:__subpackages__"],
    deps = select({
        "@rules_go//go/platform:android": [
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:linux": [
            "@org_golang_x_sys//unix",
        ],
        "//conditions:default": [],
    }),
)
