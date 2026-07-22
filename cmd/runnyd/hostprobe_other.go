//go:build !darwin && !windows

package main

// physicalRAMGB is unknown off darwin/windows -- this build exists only for
// the pure-Go CI leg, since runnyd has no VM backend (see platform_other.go)
// on any other host. 0 disables the RAM overcommit axis.
func physicalRAMGB() uint { return 0 }
