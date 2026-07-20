//go:build windows

package main

import "golang.org/x/sys/windows/svc"

// detectLaunchContext reads the live launch context on windows: the Local
// Network gate this type otherwise exists for is darwin-only and never
// consulted here (see launchcontext.go), so this only decides logSinkFor's
// tee. svc.IsWindowsService is the same check runEntry already made in
// svc_windows.go before ever reaching run — cheap, local, side-effect-free,
// safe to repeat. An error is treated as "not a service" (today's existing
// default), never worse than the pre-launchService baseline.
func detectLaunchContext() launchContext {
	if isService, err := svc.IsWindowsService(); err == nil && isService {
		return launchService
	}
	return launchForeground
}
