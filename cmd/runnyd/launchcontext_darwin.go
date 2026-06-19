package main

import "os"

// detectLaunchContext reads the live launch context on darwin, where the
// XPC_SERVICE_NAME signal and the Local Network gate exist. ppid is read from
// os.Getppid(); the env var is launchd's per-job tag.
func detectLaunchContext() launchContext {
	return classifyLaunchContext(os.Getppid(), os.Getenv("XPC_SERVICE_NAME"))
}
