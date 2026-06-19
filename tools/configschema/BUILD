load("@rules_go//go:def.bzl", "go_binary", "go_library", "go_test")

go_library(
    name = "configschema_lib",
    srcs = [
        "main.go",
        "schema.go",
    ],
    importpath = "github.com/bojanrajkovic/runny/tools/configschema",
    visibility = ["//visibility:private"],
    deps = [
        "//internal/home",
        "@com_github_invopop_jsonschema//:jsonschema",
    ],
)

go_binary(
    name = "configschema",
    embed = [":configschema_lib"],
    visibility = ["//visibility:public"],
)

go_test(
    name = "configschema_test",
    srcs = ["schema_test.go"],
    embed = [":configschema_lib"],
    embedsrcs = ["config.schema.json"],
)
