//go:build darwin

package main

import (
	"github.com/bojanrajkovic/runny/internal/statemachine"
	"github.com/bojanrajkovic/runny/internal/sysdaemon"
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

// systemRespawnTargetPath is the file naming the binary the platform supervisor
// would respawn the system daemon as -- launchd's LaunchDaemon plist here.
//
// Its EMPTINESS off darwin is the whole point: it says the platform has no
// staged-newer-binary state at all, which is what UpgradeReload's parse
// deferral depends on. Homebrew installs to a versioned cellar and repoints a
// stable opt symlink, so a newer binary genuinely exists on disk while the old
// process runs. Windows locks the image of a running process, and neither
// Chocolatey nor winget produces a versioned-path-plus-link layout, so that
// state cannot arise there -- an upgrade is stop, replace, start, performed
// from outside the daemon.
func systemRespawnTargetPath() string { return sysdaemon.PlistPath() }
