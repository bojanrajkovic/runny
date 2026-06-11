load("@rules_go//go:def.bzl", "go_library", "go_test")

go_library(
    name = "home",
    srcs = [
        "config.go",
        "home.go",
        "instance.go",
    ],
    importpath = "github.com/bojanrajkovic/runny/internal/home",
    visibility = ["//:__subpackages__"],
    deps = ["@com_github_goccy_go_yaml//:go-yaml"],
)

go_test(
    name = "home_test",
    srcs = [
        "config_test.go",
        "instance_test.go",
    ],
    embed = [":home"],
)
