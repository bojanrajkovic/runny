package main

import (
	"syscall"
	"testing"

	runnyv1 "github.com/bojanrajkovic/runny/proto/runny/v1"
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

// Only a routing error is the TCC-denial signature; a timeout (or other
// ambiguous error) must NOT read as denied — else a slow gateway at guest boot
// pops a false red "access denied" card.
func TestDeniedErr(t *testing.T) {
	if !deniedErr(syscall.EHOSTUNREACH) {
		t.Error("'no route to host' must read as denied — the TCC-denial signature")
	}
	if !deniedErr(syscall.ENETUNREACH) {
		t.Error("'network unreachable' must read as denied")
	}
	if deniedErr(syscall.ETIMEDOUT) {
		t.Error("a dial timeout must NOT read as denied — it is ambiguous, not a routing failure")
	}
	if deniedErr(syscall.ECONNREFUSED) {
		t.Error("a refused connection is reachable, not denied")
	}
	if deniedErr(nil) {
		t.Error("a successful dial is not denied")
	}
}

// Without a vmnet interface the daemon cannot determine its grant, so the honest
// classification is UNKNOWN — which the app renders as "prompt may be pending",
// never a false REACHABLE or DENIED.
func TestLocalNetworkGrantUnknownWithoutVmnet(t *testing.T) {
	if vmnetInterfaceUp() {
		t.Skip("a 192.168.64.0/24 interface is present; the live probe would run")
	}
	if got := localNetworkGrant(); got != runnyv1.LocalNetworkGrant_LOCAL_NETWORK_GRANT_UNKNOWN {
		t.Errorf("grant without vmnet = %v, want UNKNOWN", got)
	}
}

// A fresh sampler reads UNSPECIFIED (the int32 zero) until its first sample
// lands, and read() round-trips whatever the sampler stored — so the status hot
// path never blocks on the probe.
func TestLocalNetworkSamplerReadRoundTrips(t *testing.T) {
	s := &localNetworkSampler{}
	if got := s.read(); got != runnyv1.LocalNetworkGrant_LOCAL_NETWORK_GRANT_UNSPECIFIED {
		t.Errorf("fresh sampler read = %v, want UNSPECIFIED", got)
	}
	s.grant.Store(int32(runnyv1.LocalNetworkGrant_LOCAL_NETWORK_GRANT_REACHABLE))
	if got := s.read(); got != runnyv1.LocalNetworkGrant_LOCAL_NETWORK_GRANT_REACHABLE {
		t.Errorf("sampler read after store = %v, want REACHABLE", got)
	}
}
