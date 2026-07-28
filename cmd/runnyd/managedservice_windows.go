package main

import "golang.org/x/sys/windows/svc"

// runningAsManagedService reports whether the SCM started this process.
//
// There is no uid analogue here: os.Geteuid() returns -1 on windows, which is
// why the uid-shaped guard this replaces could never fire on the platform. The
// account question is answered separately by home.ResolveServer's ownership
// probe; this one answers whether we were SUPPOSED to be on the system home.
// Both are needed — ownership alone would refuse a human running a per-user
// daemon beside a system install, which is a supported shape.
//
// A probe failure reports false: refusing to start a daemon because we could
// not classify our own launch context would be a worse failure than the
// misconfiguration this guard reports.
func runningAsManagedService() bool {
	isSvc, err := svc.IsWindowsService()
	return err == nil && isSvc
}
