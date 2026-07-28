//go:build !windows

package main

import "os"

// runningAsManagedService reports whether a service manager started this
// process. On unix that is the service-account uid range: launchd's system
// daemons run below the 500 login-user floor. Root is excluded — not this
// project's deployment model, and it owns everything anyway.
func runningAsManagedService() bool { return managedServiceUID(os.Geteuid()) }

// managedServiceUID is the pure half, so the range is testable without being
// a service.
func managedServiceUID(euid int) bool { return euid > 0 && euid < 500 }
