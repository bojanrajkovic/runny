package main

import (
	"syscall"
	"testing"
)

func TestReachableErr(t *testing.T) {
	if !reachableErr(nil) {
		t.Error("a successful dial must read as reachable")
	}
	if !reachableErr(syscall.ECONNREFUSED) {
		t.Error("a refused connection (RST) must read as reachable")
	}
	if reachableErr(syscall.EHOSTUNREACH) {
		t.Error("'no route to host' must NOT read as reachable — it is the TCC-denial signature")
	}
	if reachableErr(syscall.ENETUNREACH) {
		t.Error("'network unreachable' must NOT read as reachable")
	}
}

// On a host with no vmnet interface (any CI box, the Linux dev box), the
// probe must stay quiet rather than false-alarm.
func TestLocalNetworkReachSkipsWithoutVmnet(t *testing.T) {
	if vmnetInterfaceUp() {
		t.Skip("a 192.168.64.0/24 interface is present; the live probe would run")
	}
	ok, detail := localNetworkReach()
	if !ok {
		t.Errorf("idle host without vmnet should not fail the check: %q", detail)
	}
}
