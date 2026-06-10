load("@rules_go//go:def.bzl", "go_library", "go_test")

go_library(
    name = "sshx",
    srcs = ["sshx.go"],
    importpath = "github.com/bojanrajkovic/runny/internal/sshx",
    visibility = ["//:__subpackages__"],
    deps = [
        "//internal/bounded",
        "@org_golang_x_crypto//ssh",
    ],
)

go_test(
    name = "sshx_test",
    srcs = ["sshx_test.go"],
    embed = [":sshx"],
    deps = [
        "//internal/bounded",
        "@org_golang_x_crypto//ssh",
    ],
)
