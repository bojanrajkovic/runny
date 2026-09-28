//go:build !windows

package oci

// checkHostDiskFormat is a no-op off windows: Hyper-V is the only backend
// with no attach path for ASIF at all (see headroom_windows.go for the
// analogous windows-only split).
func checkHostDiskFormat(string) error { return nil }
