//go:build !windows

package main

import (
	"fmt"
	"os"
	"syscall"
)

// fileOwnerUID returns the uid that owns path.
func fileOwnerUID(path string) (uint32, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("stat %s: no unix owner info", path)
	}
	return st.Uid, nil
}
