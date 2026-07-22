//go:build windows

package vm

import "testing"

func TestFindPermanentIP(t *testing.T) {
	entries := []neighborEntry{
		{ip: "172.20.144.1", mac: "00-15-5d-dd-3e-de", state: 5}, // Reachable, not Permanent
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

	// Reachable (not Permanent) never matches, even with the right MAC --
	// WaitIP only trusts the HNS-programmed static entry, never an
	// ARP-learned one.
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
