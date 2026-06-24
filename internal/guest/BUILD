load("@rules_go//go:def.bzl", "go_library", "go_test")

go_library(
    name = "guest",
    srcs = ["guest.go"],
    importpath = "github.com/bojanrajkovic/runny/internal/guest",
    visibility = ["//:__subpackages__"],
    deps = [
        "//internal/bounded",
        "//internal/sshx",
        "//internal/statemachine",
        "@org_golang_x_crypto//ssh",
    ],
)

go_test(
    name = "guest_test",
    srcs = [
        "guest_test.go",
        "stop_runner_shell_test.go",
    ],
    embed = [":guest"],
    deps = [
        "//internal/bounded",
        "//internal/sshx",
        "@org_golang_x_crypto//ssh",
    ],
)
