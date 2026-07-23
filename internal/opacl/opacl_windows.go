//go:build windows

package opacl

import (
	"fmt"
	"os/exec"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/bojanrajkovic/runny/internal/bounded"
)

// grantArgs is the icacls sequence Grant runs, a pure function for the same
// testability reason internal/sysdaemon's icaclsHomeArgs is one. It stamps only
// the home dir: the control channel is a named pipe (no filesystem object to
// grant), so unlike darwin there is no separate live-socket target. The home
// ACE is exactly the install bootstrap's operator grant — (OI)(CI)M, Modify
// inherited by every file and directory created beneath — so a
// bootstrap-granted and a live-granted operator are indistinguishable to
// ListIDs.
func grantArgs(homeDir, account string) [][]string {
	return [][]string{
		{"icacls", homeDir, "/grant", account + ":(OI)(CI)M"},
	}
}

// revokeArgs mirrors grantArgs: the home dir only — the ACL the revocation
// gate reads.
func revokeArgs(homeDir, account string) [][]string {
	return [][]string{
		{"icacls", homeDir, "/remove:g", account},
	}
}

// Grant adds the inheriting operator ACE for account to homeDir via icacls —
// executed bare (resolved from PATH), matching the install bootstrap's own
// icacls calls. sock is unused: the windows control channel is a named pipe
// with no file to stamp (the signature matches darwin's for the shared apply
// seam in internal/socket).
func Grant(ctx bounded.Context, homeDir, sock, account string) error {
	return runIcacls(ctx, grantArgs(homeDir, account))
}

// Revoke removes account's operator ACE from homeDir. sock is unused (see
// Grant).
func Revoke(ctx bounded.Context, homeDir, sock, account string) error {
	return runIcacls(ctx, revokeArgs(homeDir, account))
}

func runIcacls(ctx bounded.Context, cmds [][]string) error {
	for _, args := range cmds {
		out, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// ListIDs reads homeDir's DACL and returns the SID string of every operator
// ACE, skipping the per-entry username resolution List does — the same
// no-directory-service-latency property the per-RPC revocation gate relies
// on with the darwin implementation (LookupAccountSid against the local
// machine is the one unavoidable per-ACE call; see operatorSIDs).
func ListIDs(homeDir string) ([]string, error) {
	sd, err := windows.GetNamedSecurityInfo(homeDir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil, fmt.Errorf("reading %s's security descriptor: %w", homeDir, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return nil, fmt.Errorf("reading %s's DACL: %w", homeDir, err)
	}
	return operatorSIDs(dacl)
}

// operatorSIDs walks a DACL and applies the operator membership rule,
// validated against a real installed system daemon's home DACL: an
// ACCESS_ALLOWED ACE, not INHERIT_ONLY (an inherit-only ACE grants nothing
// on the directory itself), whose mask carries FILE_WRITE_DATA (the
// operator-defining bit, exactly like darwin's ACL_WRITE_DATA check — it is
// what lets a principal write to inherited artifacts), for a SID that is not a
// well-known / service principal.
//
// The well-known exclusion is structural — by SID-string prefix, BEFORE
// trusting LookupAccountSid's type — because that type check is not reliable:
// in the daemon's own process context LookupAccountSid reports the service SID
// `NT SERVICE\runnyd` (S-1-5-80-*) as SidTypeUser, which would slip the
// bootstrap's own service ACE into the operator set — the daemon counting
// itself as an operator. excludedSID drops SYSTEM/LOCAL SERVICE/NETWORK
// SERVICE, the S-1-5-32- built-in aliases
// (Administrators, Users, ...), and the S-1-5-80- service range up front. The
// SidTypeUser check is kept as a secondary filter: it still drops a SID that no
// longer resolves at all (a deleted account), which the grant/revoke path
// cannot act on anyway. The walk is bounded by the DACL's own uint16 ACE count;
// no separate cap needed.
func operatorSIDs(dacl *windows.ACL) ([]string, error) {
	if dacl == nil {
		// A nil DACL means "everything allowed" via no entries at all — not
		// a state the install bootstrap ever writes, and nothing enumerable.
		return nil, nil
	}
	var ids []string
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return nil, fmt.Errorf("reading ACE %d: %w", i, err)
		}
		if !operatorACEShape(ace.Header.AceType, ace.Header.AceFlags, uint32(ace.Mask)) {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		s := sid.String()
		if excludedSID(s) {
			continue
		}
		if _, _, accType, err := sid.LookupAccount(""); err != nil || accType != windows.SidTypeUser {
			continue
		}
		ids = append(ids, s)
	}
	return ids, nil
}

// excludedSID reports whether a SID string can never be an operator: the
// well-known singletons SYSTEM (S-1-5-18), LOCAL SERVICE (S-1-5-19), and
// NETWORK SERVICE (S-1-5-20); the S-1-5-32- built-in alias range
// (Administrators, Users, and the rest); and the S-1-5-80- service SID range
// (`NT SERVICE\*`, including the daemon's own account). Prefix-matched so it is
// independent of LookupAccountSid's type verdict, which misreports the service
// SID as a user in the daemon's context.
func excludedSID(sid string) bool {
	switch sid {
	case "S-1-5-18", "S-1-5-19", "S-1-5-20":
		return true
	}
	return strings.HasPrefix(sid, "S-1-5-32-") || strings.HasPrefix(sid, "S-1-5-80-")
}

// operatorACEShape is the type/flags/mask half of the membership rule, kept
// pure (no syscalls) so it is unit-testable against synthetic ACE data; the
// SidTypeUser half needs a live account lookup and stays in operatorSIDs.
func operatorACEShape(aceType, aceFlags uint8, mask uint32) bool {
	return aceType == windows.ACCESS_ALLOWED_ACE_TYPE &&
		aceFlags&windows.INHERIT_ONLY_ACE == 0 &&
		mask&windows.FILE_WRITE_DATA != 0
}
