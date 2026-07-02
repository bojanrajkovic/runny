load("@rules_go//go:def.bzl", "go_library", "go_test")

go_library(
    name = "obs",
    srcs = [
        "http.go",
        "obs.go",
    ],
    importpath = "github.com/bojanrajkovic/runny/internal/obs",
    visibility = ["//:__subpackages__"],
)

go_test(
    name = "obs_test",
    srcs = [
        "http_test.go",
        "obs_test.go",
    ],
    embed = [":obs"],
    deps = ["//internal/bounded"],
)
