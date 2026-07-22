//go:build !windows

package telemetry

import "go.opentelemetry.io/otel/trace"

// installOpenCensusBridge is a no-op off windows: nothing in this codebase
// produces OpenCensus spans there.
func installOpenCensusBridge(trace.TracerProvider) {}
