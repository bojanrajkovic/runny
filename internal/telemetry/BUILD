load("@rules_go//go:def.bzl", "go_library", "go_test")

go_library(
    name = "telemetry",
    srcs = [
        "metrics.go",
        "telemetry.go",
        "trace.go",
    ],
    importpath = "github.com/bojanrajkovic/runny/internal/telemetry",
    visibility = ["//:__subpackages__"],
    deps = [
        "//internal/diskfree",
        "//internal/home",
        "//internal/images",
        "//internal/obs",
        "//internal/traceid",
        "@io_opentelemetry_go_otel//:otel",
        "@io_opentelemetry_go_otel//attribute",
        "@io_opentelemetry_go_otel//codes",
        "@io_opentelemetry_go_otel//semconv/v1.28.0:v1_28_0",
        "@io_opentelemetry_go_otel_exporters_otlp_otlpmetric_otlpmetricgrpc//:otlpmetricgrpc",
        "@io_opentelemetry_go_otel_exporters_otlp_otlptrace_otlptracegrpc//:otlptracegrpc",
        "@io_opentelemetry_go_otel_metric//:metric",
        "@io_opentelemetry_go_otel_sdk//resource",
        "@io_opentelemetry_go_otel_sdk//trace",
        "@io_opentelemetry_go_otel_sdk_metric//:metric",
        "@io_opentelemetry_go_otel_trace//:trace",
    ],
)

go_test(
    name = "telemetry_test",
    srcs = [
        "metrics_test.go",
        "telemetry_test.go",
        "trace_test.go",
    ],
    embed = [":telemetry"],
    deps = [
        "//internal/home",
        "//internal/obs",
        "//internal/traceid",
        "@io_opentelemetry_go_otel//:otel",
        "@io_opentelemetry_go_otel//attribute",
        "@io_opentelemetry_go_otel//codes",
        "@io_opentelemetry_go_otel_metric//:metric",
        "@io_opentelemetry_go_otel_sdk//trace",
        "@io_opentelemetry_go_otel_sdk//trace/tracetest",
        "@io_opentelemetry_go_otel_sdk_metric//:metric",
        "@io_opentelemetry_go_otel_sdk_metric//metricdata",
        "@io_opentelemetry_go_otel_trace//:trace",
    ],
)
