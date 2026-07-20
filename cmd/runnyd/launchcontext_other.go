//go:build !darwin && !windows

package main

// detectLaunchContext is darwin-only in meaning: XPC_SERVICE_NAME and the Local
// Network gate are macOS concepts, and ppid==1 off macOS is a normal container
// init, not a self-daemonize. Off darwin (and off windows, which has its own
// SCM-aware detector) the launch context is never consulted for a real
// decision (the local-network check is darwin-gated, and there's no service
// manager here to avoid double-logging into), so report the benign foreground.
func detectLaunchContext() launchContext {
	return launchForeground
}
