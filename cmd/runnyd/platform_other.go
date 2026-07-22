//go:build !darwin && !windows

package main

import (
	"errors"

	"github.com/bojanrajkovic/runny/internal/bounded"
	"github.com/bojanrajkovic/runny/internal/statemachine"
	"github.com/bojanrajkovic/runny/internal/tart"
	"github.com/bojanrajkovic/runny/internal/vm"
)

// unsupportedManager keeps the daemon buildable (and -doctor useful) on
// hosts with no VM backend; Boot always fails with a clear error.
type unsupportedManager struct{}

func (unsupportedManager) Boot(bounded.Context, tart.Bundle, vm.BootOptions) (vm.Machine, error) {
	return nil, vm.ErrUnsupportedPlatform
}

func vmManager() vm.Manager { return unsupportedManager{} }

func cloner() statemachine.Cloner {
	return func(tart.Bundle, string) error {
		return errors.New("clone unsupported: no VM backend on this platform")
	}
}

// vmPreflight is windows-specific (see platform_windows.go); not applicable
// here, so the doctor's caller never surfaces this check on this platform.
func vmPreflight() (bool, string) { return true, "" }
