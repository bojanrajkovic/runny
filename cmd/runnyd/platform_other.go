//go:build !darwin

package main

import (
	"github.com/bojanrajkovic/runny/internal/bounded"
	"github.com/bojanrajkovic/runny/internal/tart"
	"github.com/bojanrajkovic/runny/internal/vm"
)

// unsupportedManager keeps the daemon buildable (and -doctor useful) on
// non-darwin hosts; Boot always fails with a clear error.
type unsupportedManager struct{}

func (unsupportedManager) Boot(bounded.Context, tart.Bundle, vm.BootOptions) (vm.Machine, error) {
	return nil, vm.ErrUnsupportedPlatform
}

func vmManager() vm.Manager { return unsupportedManager{} }
