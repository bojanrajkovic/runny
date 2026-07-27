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

func (unsupportedManager) ReapOrphans(string, string) error { return nil }

func vmManager() vm.Manager { return unsupportedManager{} }

func cloner() statemachine.Cloner {
	return func(tart.Bundle, string) error {
		return errors.New("clone unsupported: no VM backend on this platform")
	}
}

// vmPreflight is windows-specific (see platform_windows.go); not applicable
// here, so the doctor's caller never surfaces this check on this platform.
func vmPreflight() (bool, string) { return true, "" }

// vmBackendName identifies the VM backend for the telemetry resource
// attribute (see telemetry.Setup's backend param) — a build with no real
// backend still reports one, so a fleet's traces/metrics never show an
// empty/missing value for a host that's simply unsupported.
func vmBackendName() string { return "unsupported" }

// systemRespawnTargetPath: none on this platform. See the darwin
// implementation for why an empty path is a statement about the platform, not
// a missing feature -- a running executable cannot be replaced in place here,
// so no newer binary can be staged at the path the supervisor would respawn.
func systemRespawnTargetPath() string { return "" }
