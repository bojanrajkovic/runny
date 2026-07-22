//go:build windows

package telemetry

import (
	"go.opentelemetry.io/otel/bridge/opencensus"
	"go.opentelemetry.io/otel/trace"
)

// installOpenCensusBridge redirects go.opencensus.io/trace spans -- the
// vendored winhcs tree's own span API (internal/winhcs/oc) -- into tp, so
// the Hyper-V backend's compute-system create/start/shutdown spans land in
// runny's own OTel pipeline instead of going nowhere (OpenCensus's own
// default tracer is a no-op). windows-only: winhcs is the only OpenCensus
// span producer in this codebase, and it's windows-only itself.
func installOpenCensusBridge(tp trace.TracerProvider) {
	opencensus.InstallTraceBridge(opencensus.WithTracerProvider(tp))
}
