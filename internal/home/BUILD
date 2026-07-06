load("@rules_go//go:def.bzl", "go_library", "go_test")

go_library(
    name = "home",
    srcs = [
        "config.go",
        "home.go",
        "instance.go",
        "operatorgrants.go",
        "testconfig.go",
        "warnings.go",
    ],
    importpath = "github.com/bojanrajkovic/runny/internal/home",
    visibility = ["//:__subpackages__"],
    deps = [
        "@com_github_goccy_go_yaml//:go-yaml",
        "@org_golang_x_sys//unix",
    ],
)

go_test(
    name = "home_test",
    srcs = [
        "config_test.go",
        "instance_test.go",
        "operatorgrants_test.go",
        "resolve_test.go",
        "warnings_test.go",
    ],
    embed = [":home"],
)
