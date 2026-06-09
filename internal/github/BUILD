load("@rules_go//go:def.bzl", "go_library", "go_test")

go_library(
    name = "github",
    srcs = ["github.go"],
    importpath = "github.com/bojanrajkovic/runny/internal/github",
    visibility = ["//:__subpackages__"],
    deps = ["@com_github_golang_jwt_jwt_v5//:jwt"],
)

go_test(
    name = "github_test",
    srcs = ["github_test.go"],
    embed = [":github"],
    deps = ["@com_github_golang_jwt_jwt_v5//:jwt"],
)
