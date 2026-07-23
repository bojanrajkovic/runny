package vm

import (
	"net"
	"strings"
)

// parseInetIP extracts the guest's IPv4 address from `ip -4 -o addr show eth0`
// console output -- the address the guest itself reports, which fixupNetwork
// treats as the authoritative lease (see hcs_windows.go's WaitIP for why the
// host neighbor table's Permanent row can't be trusted for it). The address
// appears as an `inet <a.b.c.d>/<prefix>` token; matching the bare word "inet"
// (not "inet6") after whitespace-splitting naturally skips the IPv6 line, and
// net.ParseCIDR strips the prefix. Pure and untagged so it's tested off real
// hardware; the console I/O it parses is windows-only (netfixup_windows.go).
func parseInetIP(consoleOutput string) (string, bool) {
	fields := strings.Fields(consoleOutput)
	for i, f := range fields {
		if f != "inet" || i+1 >= len(fields) {
			continue
		}
		ip, _, err := net.ParseCIDR(fields[i+1])
		if err != nil {
			continue
		}
		if v4 := ip.To4(); v4 != nil {
			return v4.String(), true
		}
	}
	return "", false
}
