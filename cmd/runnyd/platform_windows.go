//go:build windows

package main

import (
	"fmt"
	"runtime"

	"github.com/bojanrajkovic/runny/internal/statemachine"
	"github.com/bojanrajkovic/runny/internal/tart"
	"github.com/bojanrajkovic/runny/internal/vm"
	"github.com/bojanrajkovic/runny/internal/winhcs/hcn"
	"github.com/bojanrajkovic/runny/internal/winhcs/osversion"
)

func vmManager() vm.Manager { return vm.HCSManager{} }

func cloner() statemachine.Cloner {
	return func(src tart.Bundle, dst string) error {
		_, err := tart.CloneVHDX(src, tart.Bundle(dst))
		return err
	}
}

// vmPreflight checks the real preconditions HCSManager.Boot requires beyond
// a bare GOOS/GOARCH match — the doctor's "platform" check can't see these
// (osversion/hcn are windows-only packages, unreachable from cmd/runnyd's
// portable code), so a host below the schema-2.1 floor or with no resolvable
// Default Switch used to report a clean bill of health right up until the
// first real Boot failed deep in the FSM instead.
func vmPreflight() (bool, string) {
	if osversion.Build() < osversion.RS5 {
		return false, fmt.Sprintf("Windows build %d is below the Hyper-V VM backend's schema-2.1 floor (build %d)", osversion.Build(), osversion.RS5)
	}
	if _, err := hcn.GetNetworkByName("Default Switch"); err != nil {
		return false, fmt.Sprintf("Default Switch not resolvable: %v", err)
	}
	return true, fmt.Sprintf("windows/%s, build %d, Default Switch resolvable", runtime.GOARCH, osversion.Build())
}

// vmBackendName identifies the VM backend for the telemetry resource
// attribute (see telemetry.Setup's backend param).
func vmBackendName() string { return "hcs" }

// systemRespawnTargetPath: none on this platform. See the darwin
// implementation for why an empty path is a statement about the platform, not
// a missing feature -- a running executable cannot be replaced in place here,
// so no newer binary can be staged at the path the supervisor would respawn.
func systemRespawnTargetPath() string { return "" }
