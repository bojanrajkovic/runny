package main

import (
	"errors"
	"net"
	"syscall"
	"time"
)

// vmnetGateway is the default gateway of Apple's NAT/vmnet guest subnet.
const vmnetGateway = "192.168.64.1"

var vmnetSubnet = func() *net.IPNet {
	_, n, _ := net.ParseCIDR("192.168.64.0/24")
	return n
}()

// localNetworkReach probes whether THIS process can reach the guest subnet.
// A LaunchDaemon-launched or background-reparented runnyd is silently denied
// vmnet / Local-Network access by macOS TCC: guest dials fail "no route to
// host" while the host shell reaches the same address (docs/deploy.md). The
// probe only asserts when a vmnet interface is actually up — one appears on
// the first guest boot — and otherwise reports that it could not verify yet,
// so a fresh idle daemon never raises a false alarm. Only meaningful on
// darwin; the caller gates on GOOS.
func localNetworkReach() (ok bool, detail string) {
	if !vmnetInterfaceUp() {
		return true, "no active vmnet yet — boot a guest, then re-run doctor to confirm the Local Network grant"
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(vmnetGateway, "1"), 2*time.Second)
	if conn != nil {
		_ = conn.Close()
	}
	if reachableErr(err) {
		return true, "guest subnet reachable"
	}
	return false, "cannot reach the guest subnet — if the host shell can, this process lacks the Local Network (TCC) grant; see docs/deploy.md (err: " + err.Error() + ")"
}

// reachableErr: a successful dial OR a refused connection both prove the
// subnet is routable from this process (an RST means "reachable, nothing
// listening"). A routing error — no route to host, network unreachable — is
// the TCC-denial signature.
func reachableErr(err error) bool {
	return err == nil || errors.Is(err, syscall.ECONNREFUSED)
}

func vmnetInterfaceUp() bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && vmnetSubnet.Contains(ipn.IP) {
			return true
		}
	}
	return false
}
