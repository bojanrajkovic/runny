package vm

import "slices"

// The IP-neighbor-table selection logic WaitIP (hcs_windows.go) runs is pure
// -- it decides which row to trust given a MAC and a snapshot of rows -- so it
// lives here, untagged, tested on any host, apart from the windows-only
// GetIpNetTable2 syscall glue (neighbortable_windows.go) that produces those
// rows. neighborEntry is this package's own view of a MIB_IPNET_ROW2 row: just
// the fields WaitIP and teardown need.
type neighborEntry struct {
	ip             string
	mac            string
	state          int32
	interfaceIndex uint32
}

// NL_NEIGHBOR_STATE values (nldef.h), in the enumeration order Microsoft Learn
// documents for MIB_IPNET_ROW2.State:
// https://learn.microsoft.com/windows/win32/api/netioapi/ns-netioapi-mib_ipnet_row2
// nlnsPermanent (6) is the state HNS writes for a bare-HCS endpoint's binding
// -- empirically confirmed via this backend's spike (an HCS-created endpoint's
// binding reads State=6 through Get-NetNeighbor). Reachable/Stale are the
// dynamically-learned states an ordinary ARP/ND resolution would produce.
const (
	nlnsUnreachable = 0
	nlnsIncomplete  = 1
	nlnsProbe       = 2
	nlnsDelay       = 3
	nlnsStale       = 4
	nlnsReachable   = 5
	nlnsPermanent   = 6
)

func (e neighborEntry) permanent() bool { return e.state == nlnsPermanent }

// learned reports whether this row is a dynamically-resolved lease (a state
// that carries a confirmed address HNS *learned*, rather than the Permanent
// pre-commit it writes at endpoint-attach before the guest has booted). Only
// Reachable/Stale qualify -- the transitional states (Probe/Delay/Incomplete)
// may not yet have a settled address.
func (e neighborEntry) learned() bool {
	return e.state == nlnsReachable || e.state == nlnsStale
}

// permanentEntriesForMAC returns every Permanent row for mac, in table order.
// These are HNS *pre-commits* -- the MAC->IP bindings HNS writes at
// endpoint-attach. A divergent boot leaves MORE THAN ONE for a single MAC (the
// stale pre-commit plus the real lease, both written Permanent), so teardown's
// scrub must delete all of them, not just the first, or the leftovers
// accumulate one stale row per boot. MACs are normalized the same way WaitIP's
// darwin sibling matches dhcpd_leases entries (leases.go's normalizeMAC) --
// Windows renders MACs dash-separated, macOS colon-separated, so both sides
// always go through the same normalizer.
func permanentEntriesForMAC(entries []neighborEntry, mac string) []neighborEntry {
	want := normalizeMAC(mac)
	var out []neighborEntry
	for _, e := range entries {
		if e.permanent() && normalizeMAC(e.mac) == want {
			out = append(out, e)
		}
	}
	return out
}

// permanentIPs returns just the IPs of permanentEntriesForMAC -- the shape
// divergentPermanentIPs needs (it compares against the console lease, which has
// no interface index).
func permanentIPs(entries []neighborEntry, mac string) []string {
	var ips []string
	for _, e := range permanentEntriesForMAC(entries, mac) {
		ips = append(ips, e.ip)
	}
	return ips
}

// divergentPermanentIPs returns the Permanent-row IPs for mac that differ from
// leaseIP -- the stale pre-commit(s) the daemon would have dialed if it trusted
// the neighbor table instead of the console-observed lease. Empty means no
// divergence (the table agrees with the lease, or has no other Permanent row).
// WaitIP calls this on a *post*-fixup table read: a fresh guest has no
// pre-commit row for its MAC yet at grace-elapse, so the divergence only becomes
// visible once DHCP has settled and HNS's stale rows are in the table.
func divergentPermanentIPs(entries []neighborEntry, mac, leaseIP string) []string {
	var other []string
	for _, ip := range permanentIPs(entries, mac) {
		if ip != leaseIP {
			other = append(other, ip)
		}
	}
	return other
}

// learnedLeaseIP returns the guest's address from a neighbor-table snapshot,
// accepting ONLY a dynamically-learned row (Reachable/Stale) -- an actual ARP
// resolution to a reachable host. It deliberately never returns a Permanent
// row: HNS's Permanent entry is a pre-boot *pre-commit*, a guess the guest's
// own DHCP client routinely overrides (the exact divergence this backend's
// fixup exists to correct), so trusting it as the lease would dial a stale IP.
//
// WaitIP calls this on the grace-period fast path: a guest that genuinely
// self-configures shows up as a learned row and WaitIP returns before the
// fixup. On the currently-validated host HNS surfaces no learned row at all
// (every row is Permanent), so this finds nothing, grace elapses, and the
// fixup derives the authoritative address from the console instead.
//
// Among multiple learned rows the choice is arbitrary but must be stable:
// neighborEntry carries no recency signal, so "the newest lease" isn't
// available -- the lexicographically smallest IP is a deterministic stand-in
// that never depends on table iteration order.
func learnedLeaseIP(entries []neighborEntry, mac string) (string, bool) {
	want := normalizeMAC(mac)
	var ip string
	var found bool
	for _, e := range entries {
		if e.learned() && normalizeMAC(e.mac) == want && (!found || e.ip < ip) {
			ip, found = e.ip, true
		}
	}
	return ip, found
}

// permanentLeaseIP returns the guest's address from a neighbor-table
// snapshot, accepting HNS's own Permanent pre-commit row as authoritative —
// the opposite trust decision from learnedLeaseIP above. That's deliberate,
// not a contradiction: learnedLeaseIP exists because the currently-validated
// *Linux* image's netplan mismatches hv_netvsc's eth0 naming, so its
// Permanent pre-commit routinely diverges from the guest's real DHCP lease
// (see hcs_windows.go's WaitIP doc comment). A *Windows* guest has no such
// mismatch — Spike B proved HNS's pre-commit IS the guest's real lease for
// Windows (0 divergence across 4 concurrent boots, ARP-confirmed) — so
// hcs_windows.go's Windows WaitIP path can trust this row directly, with no
// grace period and no console fixup. Among multiple Permanent rows for one
// MAC (not observed for Windows in the spike, but permanentEntriesForMAC's
// own doc notes a divergent boot can leave more than one), the
// lexicographically smallest IP is the same deterministic stand-in
// learnedLeaseIP uses — neighborEntry carries no recency signal.
func permanentLeaseIP(entries []neighborEntry, mac string) (string, bool) {
	ips := permanentIPs(entries, mac)
	if len(ips) == 0 {
		return "", false
	}
	return slices.Min(ips), true
}
