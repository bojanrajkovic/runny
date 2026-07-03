// Package telemetry is runny's only OTEL importer, so domain packages never
// take an SDK dependency of their own. It installs OTLP trace and metric
// providers when the daemon's observability.otlp config block names a
// collector endpoint, and installs nothing — leaving the SDK's global
// no-op providers in place — when it doesn't. This package owns providers,
// resource attribution, and bounded shutdown only; turning obs.Event into
// spans and metrics is a separate consumer that installs onto the
// providers set up here.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.28.0"

	"github.com/bojanrajkovic/runny/internal/home"
)

// Shutdown flushes and stops installed providers within the caller's
// context deadline. Setup always returns a non-nil Shutdown, even when
// telemetry is off, so callers can defer it unconditionally.
type Shutdown func(context.Context) error

func noop(context.Context) error { return nil }

// Setup installs OTLP providers per cfg and returns a Shutdown to run at
// daemon exit. cfg.Endpoint == "" (telemetry off, the default) installs
// nothing and returns noop — no SDK, no goroutines, no egress. Exporter and
// SDK errors (export failures, dropped batches) are never silent: they
// route to log through otel.SetErrorHandler. Setup installs process-global
// OTEL providers, so call it at most once per process — cmd/runnyd does, at
// startup, before any goroutine could observe the providers mid-swap.
func Setup(ctx context.Context, cfg home.OTLPConfig, version, instanceID string, log *slog.Logger) (Shutdown, error) {
	if !cfg.Enabled() {
		return noop, nil
	}

	u, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return noop, fmt.Errorf("telemetry: parsing endpoint %q: %w", cfg.Endpoint, err)
	}
	insecure := u.Scheme != "https"

	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		log.Error("otel error", "err", err)
	}))

	hostname, err := os.Hostname()
	if err != nil {
		log.Warn("telemetry: hostname lookup failed; host.name attribute will be empty", "err", err)
	}
	res, err := resource.New(ctx, resource.WithAttributes(
		semconv.ServiceName("runnyd"),
		semconv.ServiceVersion(version),
		semconv.ServiceInstanceID(instanceID),
		semconv.HostName(hostname),
	))
	if err != nil {
		return noop, fmt.Errorf("telemetry: building resource: %w", err)
	}

	// Headers arrive already env-expanded by home.LoadConfig; an empty map
	// is a no-op.
	traceOpts := []otlptracegrpc.Option{otlptracegrpc.WithEndpointURL(cfg.Endpoint), otlptracegrpc.WithHeaders(cfg.Headers)}
	metricOpts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpointURL(cfg.Endpoint), otlpmetricgrpc.WithHeaders(cfg.Headers)}
	if insecure {
		traceOpts = append(traceOpts, otlptracegrpc.WithInsecure())
		metricOpts = append(metricOpts, otlpmetricgrpc.WithInsecure())
	}

	traceExp, err := otlptracegrpc.New(ctx, traceOpts...)
	if err != nil {
		return noop, fmt.Errorf("telemetry: creating trace exporter: %w", err)
	}
	metricExp, err := otlpmetricgrpc.New(ctx, metricOpts...)
	if err != nil {
		_ = traceExp.Shutdown(ctx)
		return noop, fmt.Errorf("telemetry: creating metric exporter: %w", err)
	}

	// Exponential (base-2) histogram aggregation for every histogram
	// instrument, so Prometheus-family backends ingest native histograms
	// instead of fixed-bucket ones.
	expHistView := metric.NewView(
		metric.Instrument{Kind: metric.InstrumentKindHistogram},
		metric.Stream{Aggregation: metric.AggregationBase2ExponentialHistogram{MaxSize: 160, MaxScale: 20}},
	)

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(traceExp), // default batcher drops on a full queue; never blocks the caller
	)
	mp := metric.NewMeterProvider(
		metric.WithResource(res),
		metric.WithReader(metric.NewPeriodicReader(metricExp, metric.WithInterval(cfg.MetricsInterval.D()))),
		metric.WithView(expHistView),
	)

	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)

	return func(ctx context.Context) error {
		return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx))
	}, nil
}
