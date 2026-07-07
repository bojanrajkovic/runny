package vm

import (
	"fmt"
	"strconv"
	"strings"
)

// leasesPath is macOS's DHCP lease database for vmnet guests.
const leasesPath = "/var/db/dhcpd_leases"

// FindIPByMAC scans dhcpd_leases content for an entry whose hw_address
// matches mac. The leases file strips leading zeros from octets
// (0a:0b:... appears as a:b:...), so comparison is numeric per octet —
// this bit the vz spike.
func FindIPByMAC(leases, mac string) (string, bool) {
	want := normalizeMAC(mac)
	var ip, hw string
	for line := range strings.Lines(leases) {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "ip_address="); ok {
			ip = v
		}
		if v, ok := strings.CutPrefix(line, "hw_address=1,"); ok {
			hw = v
		}
		if line == "}" {
			if ip != "" && hw != "" && normalizeMAC(hw) == want {
				return ip, true
			}
			ip, hw = "", ""
		}
	}
	return "", false
}

func normalizeMAC(s string) string {
	parts := strings.Split(strings.TrimSpace(s), ":")
	out := make([]string, len(parts))
	for i, p := range parts {
		n, err := strconv.ParseUint(p, 16, 8)
		if err != nil {
			return strings.ToLower(strings.TrimSpace(s))
		}
		out[i] = fmt.Sprintf("%02x", n)
	}
	return strings.Join(out, ":")
}
