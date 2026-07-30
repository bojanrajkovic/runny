//go:build !windows

package telemetry

// installOpenCensusAdapter is a no-op off windows: nothing in this codebase
// produces OpenCensus spans there.
func installOpenCensusAdapter() {}
