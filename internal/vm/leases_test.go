package vm

import "testing"

// Shape lifted from a real /var/db/dhcpd_leases on ix.
const leases = `{
	name=runner-1
	ip_address=192.168.64.4
	hw_address=1,7a:47:bb:5f:97:c9
	identifier=1,7a:47:bb:5f:97:c9
	lease=0x68479dle
}
{
	name=spike
	ip_address=192.168.64.2
	hw_address=1,62:25:1b:5:97:bf
	identifier=1,62:25:1b:5:97:bf
}
`

func TestFindIPByMAC(t *testing.T) {
	ip, ok := FindIPByMAC(leases, "7a:47:bb:5f:97:c9")
	if !ok || ip != "192.168.64.4" {
		t.Errorf("exact match: %q, %v", ip, ok)
	}

	// THE sharp edge: the file strips leading zeros. A search for the
	// fully-padded MAC must still match the stripped entry.
	ip, ok = FindIPByMAC(leases, "62:25:1b:05:97:bf")
	if !ok || ip != "192.168.64.2" {
		t.Errorf("zero-pad normalization: %q, %v", ip, ok)
	}

	// Case-insensitivity.
	ip, ok = FindIPByMAC(leases, "7A:47:BB:5F:97:C9")
	if !ok || ip != "192.168.64.4" {
		t.Errorf("case: %q, %v", ip, ok)
	}

	if _, ok := FindIPByMAC(leases, "aa:bb:cc:dd:ee:ff"); ok {
		t.Error("found an IP for an absent MAC")
	}
	if _, ok := FindIPByMAC("", "aa:bb:cc:dd:ee:ff"); ok {
		t.Error("found an IP in an empty file")
	}
}
