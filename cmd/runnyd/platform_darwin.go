//go:build darwin

package main

import (
	"github.com/bojanrajkovic/runny/internal/vm"
)

func vmManager() vm.Manager { return vm.VZManager{} }
