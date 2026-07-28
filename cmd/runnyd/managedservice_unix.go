//go:build !windows

package main

import "os"

// runningAsManagedService reports whether a service manager started this
// process, which is what makes falling back to a per-user home a
// misconfiguration rather than a supported shape.
//
// On unix that is the service-account uid range: launchd's system daemons run
// as a created account below the 500 login-user floor. Root is excluded — it
// is not this project's deployment model, and a root process owns everything
// anyway.
func runningAsManagedService() bool {
	euid := os.Geteuid()
	return euid > 0 && euid < 500
}
