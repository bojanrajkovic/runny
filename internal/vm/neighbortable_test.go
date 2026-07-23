package vm

import "testing"

func TestFindPermanentIP(t *testing.T) {
	entries := []neighborEntry{
		{ip: "172.20.144.1", mac: "00-15-5d-dd-3e-de", state: nlnsReachable}, // learned, not Permanent
		{ip: "172.20.159.244", mac: "00-15-5D-DD-30-75", state: nlnsPermanent},
		{ip: "172.20.157.175", mac: "00-15-5d-dd-35-06", state: nlnsPermanent},
	}

	ip, ok := findPermanentIP(entries, "00:15:5d:dd:30:75") // colon-separated, lowercase
	if !ok || ip != "172.20.159.244" {
		t.Errorf("colon-separated match: %q, %v", ip, ok)
	}

	ip, ok = findPermanentIP(entries, "00-15-5D-DD-35-06") // dash-separated, uppercase
	if !ok || ip != "172.20.157.175" {
		t.Errorf("dash-separated match: %q, %v", ip, ok)
	}

	// findPermanentIP is specifically the pre-commit reader: a learned
	// (non-Permanent) entry never matches, even with the right MAC.
	if _, ok := findPermanentIP(entries, "00:15:5d:dd:3e:de"); ok {
		t.Error("matched a non-Permanent entry")
	}

	if _, ok := findPermanentIP(entries, "aa:bb:cc:dd:ee:ff"); ok {
		t.Error("found an IP for an absent MAC")
	}
	if _, ok := findPermanentIP(nil, "00:15:5d:dd:30:75"); ok {
		t.Error("found an IP in an empty table")
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
