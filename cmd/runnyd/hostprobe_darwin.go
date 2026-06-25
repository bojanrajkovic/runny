//go:build darwin

package main

import "golang.org/x/sys/unix"

// physicalRAMGB returns the host's physical RAM in GiB via the darwin hw.memsize
// sysctl, or 0 if it can't be read — 0 disables the RAM overcommit axis rather
// than guessing.
func physicalRAMGB() uint {
	b, err := unix.SysctlUint64("hw.memsize")
	if err != nil || b == 0 {
		return 0
	}
	return uint(b >> 30)
}
