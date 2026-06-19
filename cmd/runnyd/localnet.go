package main

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"syscall"
	"time"

	runnyv1 "github.com/bojanrajkovic/runny/proto/runny/v1"
)

// vmnetGateway is the default gateway of Apple's NAT/vmnet guest subnet.
const vmnetGateway = "192.168.64.1"

var vmnetSubnet = func() *net.IPNet {
	_, n, _ := net.ParseCIDR("192.168.64.0/24")
	return n
}()

// localNetworkReach probes whether THIS process can reach the guest subnet, and
// short-circuits the one launch context macOS denies up front. A self-daemonized
// / reparented runnyd (one launchd did not start) is silently denied vmnet /
// Local-Network access by macOS TCC: guest dials fail "no route to host" while
// the host shell reaches the same address (docs/deploy.md). A launchd-started
// daemon of any uid, and a foreground sshd child, are auto-allowed. When the
// context is orphaned the check fails immediately; otherwise the live probe only
// asserts once a vmnet interface is up — one appears on the first guest boot —
// and otherwise reports it could not verify yet, so a fresh idle daemon never
// raises a false alarm. Only meaningful on darwin; the caller gates on GOOS.
func localNetworkReach() (ok bool, detail string) {
	if launchContextNow() == launchOrphaned {
		return false, orphanedDenyDetail
	}
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

// deniedErr: a routing failure — no route to host / network unreachable — is the
// definitive signature of a denied Local Network (TCC) grant (the host shell
// reaches the subnet; this process does not). A timeout or any other error is
// ambiguous, NOT a denial, so it must not classify as DENIED.
func deniedErr(err error) bool {
	return errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH)
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

// localNetworkGrant classifies this process's Local Network (TCC) access into
// the three states the daemon can actually distinguish — the tri-state published
// in GetStatusResponse.local_network_grant. It is the same probe localNetworkReach
// runs (self-daemonized → denied up front; else no vmnet → can't tell; vmnet up →
// reachable or denied), shaped for the app's proactive grant card rather than the
// doctor's (ok, detail) pair. There is no fourth "definitely never granted"
// state: macOS exposes no API to read a process's own grant, so absent the
// orphaned short-circuit and until a vmnet interface exists the honest answer is
// UNKNOWN, which the app treats as "prompt may be pending".
func localNetworkGrant() runnyv1.LocalNetworkGrant {
	if launchContextNow() == launchOrphaned {
		return runnyv1.LocalNetworkGrant_LOCAL_NETWORK_GRANT_DENIED
	}
	if !vmnetInterfaceUp() {
		return runnyv1.LocalNetworkGrant_LOCAL_NETWORK_GRANT_UNKNOWN
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(vmnetGateway, "1"), 2*time.Second)
	if conn != nil {
		_ = conn.Close()
	}
	switch {
	case reachableErr(err):
		return runnyv1.LocalNetworkGrant_LOCAL_NETWORK_GRANT_REACHABLE
	case deniedErr(err):
		return runnyv1.LocalNetworkGrant_LOCAL_NETWORK_GRANT_DENIED
	default:
		// A dial TIMEOUT (or any non-routing error) is NOT the TCC-denial
		// signature — the gateway may just be slow to answer right at guest boot.
		// Reporting DENIED here would pop a false red "access denied" card for a
		// grant that is actually fine; UNKNOWN shows the soft "may be pending" card
		// and self-corrects on the next sample.
		return runnyv1.LocalNetworkGrant_LOCAL_NETWORK_GRANT_UNKNOWN
	}
}

// localNetworkSampleInterval bounds how stale the published grant can be. Coarse
// on purpose: a TCC grant is a slow-changing fact, and the sampler's probe dial
// (up to 2s when a vmnet interface is up) must stay well off the status hot path.
const localNetworkSampleInterval = 15 * time.Second

// localNetworkSampler classifies the Local Network grant on a timer and caches
// the verdict, so the status snapshot reads a cached value rather than blocking
// every WatchStatus tick on a probe dial. The grant changes only when the user
// grants/revokes the TCC permission or a vmnet interface appears (first guest
// boot), so a coarse cadence stays responsive for the app's grant card while
// keeping every status response non-blocking.
type localNetworkSampler struct {
	grant atomic.Int32 // a runnyv1.LocalNetworkGrant, int32 for atomic access
	// onChange, if set, fires whenever a sample CHANGES the cached value (including
	// the first sample off the zero value). Wired to the server's watch fan-out so
	// a grant change reaches the app at once instead of waiting out the 30s
	// heartbeat — the proactive card must land before the first guest dial fails.
	onChange func()
}

// read returns the most recently sampled grant. UNSPECIFIED until the first
// sample lands (the int32 zero value), which the app renders as no affordance.
func (s *localNetworkSampler) read() runnyv1.LocalNetworkGrant {
	return runnyv1.LocalNetworkGrant(s.grant.Load())
}

// set stores a sampled value and notifies watchers only when it actually changed,
// so a steady grant doesn't spam the fan-out while a real transition reaches the
// app promptly.
func (s *localNetworkSampler) set(v int32) {
	if s.grant.Swap(v) != v && s.onChange != nil {
		s.onChange()
	}
}

// run samples once immediately, then on every tick until ctx ends. Bounded by
// design: each probe carries localNetworkGrant's 2s dial timeout, and the loop
// stops with the daemon — no unbounded operation (ADR-0011).
func (s *localNetworkSampler) run(ctx context.Context) {
	sample := func() { s.set(int32(localNetworkGrant())) }
	sample()
	t := time.NewTicker(localNetworkSampleInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sample()
		}
	}
}
