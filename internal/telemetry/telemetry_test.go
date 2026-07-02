package telemetry

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/bojanrajkovic/runny/internal/home"
)

func TestSetupOffByDefault(t *testing.T) {
	shutdown, err := Setup(context.Background(), home.OTLPConfig{}, "dev", "instance", slog.Default())
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("noop shutdown: %v", err)
	}
}

// An unreachable collector must not turn daemon shutdown into a hang: the
// grpc exporter's own connection is lazy, and Shutdown must return once the
// caller's deadline fires, not once the collector answers.
func TestShutdownBoundedOnUnreachableEndpoint(t *testing.T) {
	cfg := home.OTLPConfig{Endpoint: "http://127.0.0.1:1", MetricsInterval: home.Duration(time.Second)}
	shutdown, err := Setup(context.Background(), cfg, "dev", "instance", slog.Default())
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	const deadline = 2 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- shutdown(ctx) }()

	select {
	case <-done:
		if elapsed := time.Since(start); elapsed > deadline+time.Second {
			t.Fatalf("shutdown took %v, expected to respect the %v deadline", elapsed, deadline)
		}
	case <-time.After(deadline + time.Second):
		t.Fatal("shutdown did not return within the bounded deadline")
	}
}
