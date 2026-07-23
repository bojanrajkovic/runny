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
// testability reason internal/sysdaemon's icaclsHomeArgs is one. The home
// ACE is exactly the install bootstrap's operator grant — (OI)(CI)M, Modify
// inherited by every file and directory created beneath — so a
// bootstrap-granted and a live-granted operator are indistinguishable to
// ListIDs. Unlike darwin's copy-at-create ACL inheritance, NTFS propagates
// a newly added inheritable ACE to existing children as part of the DACL
// write itself, so the explicit stamp on the live socket is belt-and-braces
// against surprises, not load-bearing.
func grantArgs(homeDir, sock, account string) [][]string {
	return [][]string{
		{"icacls", homeDir, "/grant", account + ":(OI)(CI)M"},
		{"icacls", sock, "/grant", account + ":M"},
	}
}

// revokeArgs mirrors grantArgs: home first, then the live socket — the same
// two-target order darwin's chmodBoth uses, so a partial failure leaves the
// home ACL (the one the revocation gate reads) already mutated.
func revokeArgs(homeDir, sock, account string) [][]string {
	return [][]string{
		{"icacls", homeDir, "/remove:g", account},
		{"icacls", sock, "/remove:g", account},
	}
}

// Grant adds the inheriting operator ACE for account to homeDir and an
// explicit one to sock, via icacls — executed bare (resolved from PATH),
// matching the install bootstrap's own icacls calls.
func Grant(ctx bounded.Context, homeDir, sock, account string) error {
	return runIcacls(ctx, grantArgs(homeDir, sock, account))
}

// Revoke removes account's ACEs from homeDir and sock.
func Revoke(ctx bounded.Context, homeDir, sock, account string) error {
	return runIcacls(ctx, revokeArgs(homeDir, sock, account))
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
// what lets a principal write to the inherited socket), for a SID that
// resolves to a real user account (SidTypeUser). That last check is what
// excludes the non-operator ACEs the install bootstrap writes: the service
// SID `NT SERVICE\runnyd` and SYSTEM are SidTypeWellKnownGroup,
// Administrators is SidTypeAlias, CREATOR OWNER is inherit-only. A SID that
// no longer resolves at all (a deleted account) is likewise excluded — it
// cannot be verified as a user account, and the matching os/user lookups
// fail for it everywhere else in the grant/revoke path anyway. The walk is
// bounded by the DACL's own uint16 ACE count; no separate cap needed.
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
		if _, _, accType, err := sid.LookupAccount(""); err != nil || accType != windows.SidTypeUser {
			continue
		}
		ids = append(ids, sid.String())
	}
	return ids, nil
}

// operatorACEShape is the type/flags/mask half of the membership rule, kept
// pure (no syscalls) so it is unit-testable against synthetic ACE data; the
// SidTypeUser half needs a live account lookup and stays in operatorSIDs.
func operatorACEShape(aceType, aceFlags uint8, mask uint32) bool {
	return aceType == windows.ACCESS_ALLOWED_ACE_TYPE &&
		aceFlags&windows.INHERIT_ONLY_ACE == 0 &&
		mask&windows.FILE_WRITE_DATA != 0
}
