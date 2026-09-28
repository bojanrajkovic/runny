//go:build darwin

package vm

import "golang.org/x/sys/unix"

// hostMacOSVersion reads kern.osproductversion via sysctl -- the same string
// `sw_vers -productVersion` reports, without shelling out to it.
func hostMacOSVersion() (string, error) {
	return unix.Sysctl("kern.osproductversion")
}
