package vm

import (
	"slices"
	"testing"
)

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

func TestSelectLeaseIP(t *testing.T) {
	// The divergence case (the whole reason mechanism 1 exists): the same MAC
	// carries a stale Permanent pre-commit AND a learned lease at a different
	// address. The selector must return the learned one.
	diverged := []neighborEntry{
		{ip: "172.18.192.241", mac: "00-15-5d-13-59-30", state: nlnsPermanent}, // stale pre-commit
		{ip: "172.18.206.103", mac: "00-15-5d-13-59-30", state: nlnsReachable}, // real lease
	}
	if ip, ok := selectLeaseIP(diverged, "00:15:5d:13:59:30"); !ok || ip != "172.18.206.103" {
		t.Errorf("diverged: selectLeaseIP = %q, %v; want the learned 172.18.206.103", ip, ok)
	}
	// Order must not matter -- learned wins even when the Permanent row comes
	// second in the table.
	if ip, ok := selectLeaseIP([]neighborEntry{diverged[1], diverged[0]}, "00:15:5d:13:59:30"); !ok || ip != "172.18.206.103" {
		t.Errorf("diverged (reordered): selectLeaseIP = %q, %v; want 172.18.206.103", ip, ok)
	}

	// Multiple learned rows: the choice is arbitrary but must be stable
	// (lexicographically smallest IP), independent of table order.
	multiLearned := []neighborEntry{
		{ip: "172.18.206.103", mac: "00-15-5d-13-59-30", state: nlnsStale},
		{ip: "172.18.199.20", mac: "00-15-5d-13-59-30", state: nlnsReachable},
		{ip: "172.18.192.241", mac: "00-15-5d-13-59-30", state: nlnsPermanent},
	}
	if ip, ok := selectLeaseIP(multiLearned, "00:15:5d:13:59:30"); !ok || ip != "172.18.199.20" {
		t.Errorf("multi-learned: selectLeaseIP = %q, %v; want the smallest learned 172.18.199.20", ip, ok)
	}
	slices.Reverse(multiLearned)
	if ip, ok := selectLeaseIP(multiLearned, "00:15:5d:13:59:30"); !ok || ip != "172.18.199.20" {
		t.Errorf("multi-learned (reversed): selectLeaseIP = %q, %v; want a stable 172.18.199.20", ip, ok)
	}

	// The ADR-0026 happy path (and the currently-validated host, where HNS
	// only ever writes Permanent): only a Permanent pre-commit exists, and the
	// selector must still return it -- the guest self-configured to it.
	permanentOnly := []neighborEntry{
		{ip: "172.20.159.244", mac: "00-15-5d-dd-30-75", state: nlnsPermanent},
	}
	if ip, ok := selectLeaseIP(permanentOnly, "00:15:5D:DD:30:75"); !ok || ip != "172.20.159.244" {
		t.Errorf("permanent-only: selectLeaseIP = %q, %v; want the Permanent 172.20.159.244", ip, ok)
	}

	if _, ok := selectLeaseIP(permanentOnly, "aa:bb:cc:dd:ee:ff"); ok {
		t.Error("found a lease for an absent MAC")
	}
	if _, ok := selectLeaseIP(nil, "00:15:5d:dd:30:75"); ok {
		t.Error("found a lease in an empty table")
	}
}
