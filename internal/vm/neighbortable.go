package vm

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

// permanentIPs returns every Permanent-row IP for mac, in table order. These
// are HNS *pre-commits* -- the MAC->IP bindings HNS writes at endpoint-attach.
// They are NOT the address WaitIP dials once a fixup has observed the guest's
// real lease off the console (see hcs_windows.go); WaitIP reads them only to
// flag a divergence via divergentPermanentIPs. MACs are normalized the same way
// WaitIP's darwin sibling matches dhcpd_leases entries (leases.go's
// normalizeMAC) -- Windows renders MACs dash-separated, macOS colon-separated,
// so both sides always go through the same normalizer.
func permanentIPs(entries []neighborEntry, mac string) []string {
	want := normalizeMAC(mac)
	var ips []string
	for _, e := range entries {
		if e.permanent() && normalizeMAC(e.mac) == want {
			ips = append(ips, e.ip)
		}
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

// selectLeaseIP picks the guest's current address from a neighbor-table
// snapshot, preferring a dynamically-learned lease (Reachable/Stale) over the
// Permanent pre-commit when both exist for the same MAC -- the pre-commit can
// name a stale address the guest's DHCP client never actually took. This is
// the neighbor-table half of WaitIP's IP source (its "mechanism 1"): the clean
// re-read that would correct the divergence *if* the host ever surfaced a
// learned row at the real address.
//
// On the currently-validated host it never does -- HNS writes every row,
// including the guest's real lease, as Permanent, so a diverged MAC shows two
// Permanent rows and this selector cannot tell them apart. That is exactly why
// WaitIP treats the console-observed address as authoritative once a fixup has
// read it, and uses this selector only for the pre-fixup happy path (a guest
// that self-configures within the grace period, where the single Permanent row
// is correct by construction). The learned-preference branch is kept as the
// robust rule for any host/HNS build that does surface a learned row.
func selectLeaseIP(entries []neighborEntry, mac string) (string, bool) {
	want := normalizeMAC(mac)
	var learnedIP, permanentIP string
	var haveLearned, havePermanent bool
	for _, e := range entries {
		if normalizeMAC(e.mac) != want {
			continue
		}
		// Among multiple learned rows the choice is arbitrary but must be
		// stable: neighborEntry carries no recency signal, so "the newest
		// lease" isn't available -- the lexicographically smallest IP is a
		// deterministic stand-in that never depends on table iteration order.
		if e.learned() && (!haveLearned || e.ip < learnedIP) {
			learnedIP, haveLearned = e.ip, true
		}
		if e.permanent() && !havePermanent {
			permanentIP, havePermanent = e.ip, true
		}
	}
	if haveLearned {
		return learnedIP, true
	}
	return permanentIP, havePermanent
}
