//go:build !darwin

package main

import (
	"golang.org/x/sys/unix"

	"github.com/bojanrajkovic/runny/internal/bounded"
	"github.com/bojanrajkovic/runny/internal/tart"
	"github.com/bojanrajkovic/runny/internal/vm"
)

// unsupportedManager keeps the daemon buildable (and -doctor useful) on
// non-darwin hosts; Boot always fails with a clear error.
type unsupportedManager struct{}

func (unsupportedManager) Boot(bounded.Context, tart.Bundle, vm.BootOptions) (vm.Machine, error) {
	return nil, errNotDarwin
}

func vmManager() vm.Manager { return unsupportedManager{} }

func freeDiskGB(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize) / (1 << 30), nil
}
