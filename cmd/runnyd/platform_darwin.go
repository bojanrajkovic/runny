//go:build darwin

package main

import (
	"github.com/bojanrajkovic/runny/internal/statemachine"
	"github.com/bojanrajkovic/runny/internal/tart"
	"github.com/bojanrajkovic/runny/internal/vm"
)

func vmManager() vm.Manager { return vm.VZManager{} }

func cloner() statemachine.Cloner {
	return func(src tart.Bundle, dst string) error {
		_, err := tart.Clone(src, tart.Bundle(dst))
		return err
	}
}

// vmPreflight is windows-specific (see platform_windows.go); not applicable
// here, so the doctor's caller never surfaces this check on darwin.
func vmPreflight() (bool, string) { return true, "" }

// vmBackendName identifies the VM backend for the telemetry resource
// attribute (see telemetry.Setup's backend param).
func vmBackendName() string { return "vz" }
