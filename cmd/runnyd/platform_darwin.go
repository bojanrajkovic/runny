//go:build darwin

package main

import (
	"golang.org/x/sys/unix"

	"github.com/bojanrajkovic/runny/internal/vm"
)

func vmManager() vm.Manager { return vm.VZManager{} }

func freeDiskGB(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize) / (1 << 30), nil
}
