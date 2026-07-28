package main

import "golang.org/x/sys/windows/svc"

// runningAsManagedService reports whether the SCM started this process. There
// is no uid analogue: os.Geteuid() returns -1 on windows, which is why the
// uid-shaped guard this replaces could never fire here.
//
// A probe failure reports false — refusing to start because we could not
// classify our own launch context would be a worse failure than the
// misconfiguration the guard reports.
func runningAsManagedService() bool {
	isSvc, err := svc.IsWindowsService()
	return err == nil && isSvc
}
