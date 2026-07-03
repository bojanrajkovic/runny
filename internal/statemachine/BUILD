load("@rules_go//go:def.bzl", "go_library", "go_test")

go_library(
    name = "statemachine",
    srcs = [
        "fsm.go",
        "io.go",
        "statuscell.go",
    ],
    importpath = "github.com/bojanrajkovic/runny/internal/statemachine",
    visibility = ["//:__subpackages__"],
    deps = [
        "//internal/bounded",
        "//internal/cycle",
        "//internal/github",
        "//internal/home",
        "//internal/obs",
        "//internal/tart",
        "//internal/vm",
    ],
)

go_test(
    name = "statemachine_test",
    srcs = ["fsm_test.go"],
    embed = [":statemachine"],
    deps = [
        "//internal/bounded",
        "//internal/cycle",
        "//internal/github",
        "//internal/home",
        "//internal/obs",
        "//internal/tart",
        "//internal/vm",
    ],
)
