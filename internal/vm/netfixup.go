package vm

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"strings"
)

// sidLocalSystem owns every Hyper-V-created COM port pipe: the pipe server is
// vmcompute.exe running as NT AUTHORITY\SYSTEM, confirmed by reading a live
// guest's console SD on real hardware (owner S-1-5-18, DACL
// `O:SYG:SYD:(A;;FR;;;WD)(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;HA)...`).
const sidLocalSystem = "S-1-5-18"

// isTrustedConsoleOwner reports whether the dialed console pipe is owned by a
// principal an unprivileged squatter cannot impersonate as an object OWNER.
// Setting an object's owner to SYSTEM requires privilege a squatter does not
// have, which is what makes the owner (not the DACL) the thing worth checking:
// a squatter authors its own DACL freely, but cannot forge this.
//
// Narrower than cmd/runnyctl's isTrustedPipeOwner, which also accepts
// Administrators: that check authenticates runnyd's own control pipe, whereas
// this one authenticates Hyper-V, and Hyper-V always creates the console as
// SYSTEM. Deliberately kept separate rather than shared — same mechanism, two
// different trust policies, and widening one must not silently widen the other.
func isTrustedConsoleOwner(sidString string) bool {
	return sidString == sidLocalSystem
}

// consolePipeName builds the per-boot name of a guest's COM0 pipe.
//
// The name carries a fresh random suffix on every boot, and that is a security
// property, not cosmetics. runny does not create this pipe — it names it in the
// compute system document and Hyper-V's vmcompute.exe creates it — so runny
// cannot choose its DACL, and a name derived only from the slot (which is
// stable across cycles and derivable from config) could be pre-created by any
// local user before the guest boots. Whoever creates a pipe name first owns its
// security descriptor, so a pre-creating squatter can admit further instances
// and be handed the console dial that fixupNetwork then types the guest's SSH
// credentials into — and whose output WaitIP treats as the authoritative lease
// address. An unguessable name removes the ability to pre-create it at all;
// verifyConsoleOwner covers the residue.
func consolePipeName(systemID string) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating console pipe suffix: %w", err)
	}
	return `\\.\pipe\runny-console-` + systemID + "-" + hex.EncodeToString(b[:]), nil
}

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
