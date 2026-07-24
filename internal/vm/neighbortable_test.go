package vm

import (
	"slices"
	"testing"
)

func TestPermanentEntriesForMAC(t *testing.T) {
	entries := []neighborEntry{
		{ip: "172.20.144.1", mac: "00-15-5d-dd-3e-de", state: nlnsReachable, interfaceIndex: 7},    // learned, excluded
		{ip: "172.20.159.244", mac: "00-15-5D-DD-30-75", state: nlnsPermanent, interfaceIndex: 29}, // stale pre-commit
		{ip: "172.20.150.10", mac: "00-15-5d-dd-30-75", state: nlnsPermanent, interfaceIndex: 29},  // real lease, same MAC
	}

	// (a) A MAC with two Permanent rows -> BOTH are returned, with their
	// interface indices intact (deleteNeighborEntry needs them). This is the
	// regression the single-`return` scrub had: it deleted one and leaked the other.
	got := permanentEntriesForMAC(entries, "00:15:5d:dd:30:75") // (c) colon-lowercase query vs dash-upper rows
	want := []neighborEntry{
		{ip: "172.20.159.244", mac: "00-15-5D-DD-30-75", state: nlnsPermanent, interfaceIndex: 29},
		{ip: "172.20.150.10", mac: "00-15-5d-dd-30-75", state: nlnsPermanent, interfaceIndex: 29},
	}
	if !slices.Equal(got, want) {
		t.Errorf("permanentEntriesForMAC = %v, want both Permanent rows %v", got, want)
	}

	// (b) Learned rows are excluded even with the right MAC.
	if got := permanentEntriesForMAC(entries, "00:15:5d:dd:3e:de"); len(got) != 0 {
		t.Errorf("permanentEntriesForMAC returned a learned row: %v", got)
	}

	// (d) No match -> empty.
	if got := permanentEntriesForMAC(entries, "aa:bb:cc:dd:ee:ff"); len(got) != 0 {
		t.Errorf("permanentEntriesForMAC for an absent MAC = %v", got)
	}
	if got := permanentEntriesForMAC(nil, "00:15:5d:dd:30:75"); len(got) != 0 {
		t.Errorf("permanentEntriesForMAC of an empty table = %v", got)
	}
}

func TestPermanentIPs(t *testing.T) {
	entries := []neighborEntry{
		{ip: "172.20.144.1", mac: "00-15-5d-dd-3e-de", state: nlnsReachable},   // learned, excluded
		{ip: "172.20.159.244", mac: "00-15-5D-DD-30-75", state: nlnsPermanent}, // stale pre-commit
		{ip: "172.20.150.10", mac: "00-15-5d-dd-30-75", state: nlnsPermanent},  // another pre-commit, same MAC
		{ip: "172.20.157.175", mac: "00-15-5d-dd-35-06", state: nlnsPermanent},
	}

	// Every Permanent row for the MAC, in table order -- normalization spans
	// colon/dash and case on both sides.
	if got := permanentIPs(entries, "00:15:5d:dd:30:75"); !slices.Equal(got, []string{"172.20.159.244", "172.20.150.10"}) {
		t.Errorf("permanentIPs = %v, want both pre-commit rows in table order", got)
	}
	if got := permanentIPs(entries, "00-15-5D-DD-35-06"); !slices.Equal(got, []string{"172.20.157.175"}) {
		t.Errorf("permanentIPs (single) = %v", got)
	}

	// A learned (non-Permanent) row is never a pre-commit, even with the right MAC.
	if got := permanentIPs(entries, "00:15:5d:dd:3e:de"); len(got) != 0 {
		t.Errorf("permanentIPs returned a non-Permanent row: %v", got)
	}
	if got := permanentIPs(entries, "aa:bb:cc:dd:ee:ff"); len(got) != 0 {
		t.Errorf("permanentIPs for an absent MAC = %v", got)
	}
	if got := permanentIPs(nil, "00:15:5d:dd:30:75"); len(got) != 0 {
		t.Errorf("permanentIPs of an empty table = %v", got)
	}
}

func TestDivergentPermanentIPs(t *testing.T) {
	const lease = "172.18.206.103"

	// (a) The real post-fixup shape: a Permanent row at the lease plus a stale
	// pre-commit at a different IP -> the stale one is returned.
	twoRows := []neighborEntry{
		{ip: lease, mac: "00-15-5d-13-59-30", state: nlnsPermanent},
		{ip: "172.18.192.241", mac: "00-15-5D-13-59-30", state: nlnsPermanent},
	}
	if got := divergentPermanentIPs(twoRows, "00:15:5d:13:59:30", lease); !slices.Equal(got, []string{"172.18.192.241"}) {
		t.Errorf("two-row: divergentPermanentIPs = %v, want [172.18.192.241]", got)
	}

	// (b) The table agrees with the lease (only a Permanent row at the lease) -> empty.
	if got := divergentPermanentIPs([]neighborEntry{twoRows[0]}, "00:15:5d:13:59:30", lease); len(got) != 0 {
		t.Errorf("lease-only: divergentPermanentIPs = %v, want empty", got)
	}

	// (c) No rows for the MAC -> empty.
	if got := divergentPermanentIPs(nil, "00:15:5d:13:59:30", lease); len(got) != 0 {
		t.Errorf("empty table: divergentPermanentIPs = %v, want empty", got)
	}

	// (d) Learned rows are ignored -- only Permanent pre-commits count, even a
	// learned row at a divergent IP is not a pre-commit divergence.
	learnedDiverges := []neighborEntry{
		{ip: lease, mac: "00-15-5d-13-59-30", state: nlnsPermanent},
		{ip: "172.18.199.20", mac: "00-15-5d-13-59-30", state: nlnsReachable},
	}
	if got := divergentPermanentIPs(learnedDiverges, "00:15:5d:13:59:30", lease); len(got) != 0 {
		t.Errorf("learned-diverges: divergentPermanentIPs = %v, want empty (learned rows ignored)", got)
	}

	// (e) MAC normalization: dash-uppercase table row vs colon-lowercase query.
	if got := divergentPermanentIPs(twoRows, "00-15-5D-13-59-30", lease); !slices.Equal(got, []string{"172.18.192.241"}) {
		t.Errorf("normalization: divergentPermanentIPs = %v, want [172.18.192.241]", got)
	}
}

func TestLearnedLeaseIP(t *testing.T) {
	// (a) A Permanent-only table (today's reality) yields NO match -- a
	// Permanent row is HNS's pre-commit, never the lease -- so grace elapses
	// and the fixup runs. This is the P1 the learned-only rule fixes.
	permanentOnly := []neighborEntry{
		{ip: "172.20.159.244", mac: "00-15-5d-dd-30-75", state: nlnsPermanent},
	}
	if ip, ok := learnedLeaseIP(permanentOnly, "00:15:5D:DD:30:75"); ok {
		t.Errorf("permanent-only: learnedLeaseIP = %q, %v; want no match (never trust a pre-commit)", ip, ok)
	}

	// (b) A learned Reachable/Stale row -- a real ARP resolution -- is returned.
	learned := []neighborEntry{
		{ip: "172.18.206.103", mac: "00-15-5d-13-59-30", state: nlnsReachable},
	}
	if ip, ok := learnedLeaseIP(learned, "00:15:5d:13:59:30"); !ok || ip != "172.18.206.103" {
		t.Errorf("learned: learnedLeaseIP = %q, %v; want 172.18.206.103", ip, ok)
	}

	// (c) Both a learned row and a Permanent pre-commit for the MAC -> the
	// learned one, never the Permanent, regardless of table order.
	both := []neighborEntry{
		{ip: "172.18.192.241", mac: "00-15-5d-13-59-30", state: nlnsPermanent}, // stale pre-commit
		{ip: "172.18.206.103", mac: "00-15-5d-13-59-30", state: nlnsStale},     // real lease
	}
	if ip, ok := learnedLeaseIP(both, "00:15:5d:13:59:30"); !ok || ip != "172.18.206.103" {
		t.Errorf("both: learnedLeaseIP = %q, %v; want the learned 172.18.206.103", ip, ok)
	}
	if ip, ok := learnedLeaseIP([]neighborEntry{both[1], both[0]}, "00:15:5d:13:59:30"); !ok || ip != "172.18.206.103" {
		t.Errorf("both (reordered): learnedLeaseIP = %q, %v; want 172.18.206.103", ip, ok)
	}

	// (d) Multiple learned rows: deterministic (lexicographically smallest IP),
	// independent of table order.
	multiLearned := []neighborEntry{
		{ip: "172.18.206.103", mac: "00-15-5d-13-59-30", state: nlnsStale},
		{ip: "172.18.199.20", mac: "00-15-5d-13-59-30", state: nlnsReachable},
		{ip: "172.18.192.241", mac: "00-15-5d-13-59-30", state: nlnsPermanent},
	}
	if ip, ok := learnedLeaseIP(multiLearned, "00:15:5d:13:59:30"); !ok || ip != "172.18.199.20" {
		t.Errorf("multi-learned: learnedLeaseIP = %q, %v; want the smallest learned 172.18.199.20", ip, ok)
	}
	slices.Reverse(multiLearned)
	if ip, ok := learnedLeaseIP(multiLearned, "00:15:5d:13:59:30"); !ok || ip != "172.18.199.20" {
		t.Errorf("multi-learned (reversed): learnedLeaseIP = %q, %v; want a stable 172.18.199.20", ip, ok)
	}

	// (e) MAC normalization: dash-uppercase table row vs colon-lowercase query.
	if ip, ok := learnedLeaseIP([]neighborEntry{{ip: "10.0.0.5", mac: "00-15-5D-DD-3E-DE", state: nlnsReachable}}, "00:15:5d:dd:3e:de"); !ok || ip != "10.0.0.5" {
		t.Errorf("normalization: learnedLeaseIP = %q, %v; want 10.0.0.5", ip, ok)
	}

	if _, ok := learnedLeaseIP(learned, "aa:bb:cc:dd:ee:ff"); ok {
		t.Error("found a lease for an absent MAC")
	}
	if _, ok := learnedLeaseIP(nil, "00:15:5d:dd:30:75"); ok {
		t.Error("found a lease in an empty table")
	}
}

func TestPermanentLeaseIP(t *testing.T) {
	// (a) The single-row Windows case: the Permanent pre-commit IS the
	// lease, trusted directly with no grace period.
	single := []neighborEntry{
		{ip: "172.20.150.10", mac: "00-15-5d-dd-30-75", state: nlnsPermanent},
	}
	if ip, ok := permanentLeaseIP(single, "00:15:5D:DD:30:75"); !ok || ip != "172.20.150.10" {
		t.Errorf("single: permanentLeaseIP = %q, %v; want 172.20.150.10", ip, ok)
	}

	// (b) A learned row is never a Permanent pre-commit, even with the right MAC.
	if _, ok := permanentLeaseIP([]neighborEntry{{ip: "172.20.144.1", mac: "00-15-5d-dd-3e-de", state: nlnsReachable}}, "00:15:5d:dd:3e:de"); ok {
		t.Error("permanentLeaseIP matched a learned row")
	}

	// (c) Multiple Permanent rows for one MAC: deterministic (lexicographically
	// smallest IP), independent of table order -- mirrors learnedLeaseIP's
	// multi-row tiebreak.
	multi := []neighborEntry{
		{ip: "172.20.159.244", mac: "00-15-5D-DD-30-75", state: nlnsPermanent},
		{ip: "172.20.150.10", mac: "00-15-5d-dd-30-75", state: nlnsPermanent},
	}
	if ip, ok := permanentLeaseIP(multi, "00:15:5d:dd:30:75"); !ok || ip != "172.20.150.10" {
		t.Errorf("multi: permanentLeaseIP = %q, %v; want the smallest 172.20.150.10", ip, ok)
	}
	slices.Reverse(multi)
	if ip, ok := permanentLeaseIP(multi, "00:15:5d:dd:30:75"); !ok || ip != "172.20.150.10" {
		t.Errorf("multi (reversed): permanentLeaseIP = %q, %v; want a stable 172.20.150.10", ip, ok)
	}

	if _, ok := permanentLeaseIP(single, "aa:bb:cc:dd:ee:ff"); ok {
		t.Error("found a lease for an absent MAC")
	}
	if _, ok := permanentLeaseIP(nil, "00:15:5d:dd:30:75"); ok {
		t.Error("found a lease in an empty table")
	}
}
