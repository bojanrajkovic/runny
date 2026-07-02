load("@rules_go//go:def.bzl", "go_library", "go_test")

go_library(
    name = "telemetry",
    srcs = ["telemetry.go"],
    importpath = "github.com/bojanrajkovic/runny/internal/telemetry",
    visibility = ["//:__subpackages__"],
    deps = [
        "//internal/home",
        "@io_opentelemetry_go_otel//:otel",
        "@io_opentelemetry_go_otel//semconv/v1.28.0:v1_28_0",
        "@io_opentelemetry_go_otel_exporters_otlp_otlpmetric_otlpmetricgrpc//:otlpmetricgrpc",
        "@io_opentelemetry_go_otel_exporters_otlp_otlptrace_otlptracegrpc//:otlptracegrpc",
        "@io_opentelemetry_go_otel_sdk//resource",
        "@io_opentelemetry_go_otel_sdk//trace",
        "@io_opentelemetry_go_otel_sdk_metric//:metric",
    ],
)

go_test(
    name = "telemetry_test",
    srcs = ["telemetry_test.go"],
    embed = [":telemetry"],
    deps = ["//internal/home"],
)
