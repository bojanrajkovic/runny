//go:build windows

package opacl

import (
	"fmt"
	"log/slog"
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

// revokeArgs targets the home dir and everything beneath it. The grant is
// (OI)(CI), so it propagates outward by inheritance -- but the install
// bootstrap runs icacls /inheritance:d on logs\, which COPIES the inherited
// ACEs in place and stops future propagation. That copy is a real ACE on
// logs\, not a reflection of the home's, so removing the home's grant alone
// leaves a revoked operator holding Modify on the daemon's logs. /T walks the
// tree the grant reached.
func revokeArgs(homeDir, account string) [][]string {
	return [][]string{
		{"icacls", homeDir, "/remove:g", account, "/T"},
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
// itself as an operator. ExcludedSID drops SYSTEM/LOCAL SERVICE/NETWORK
// SERVICE, the S-1-5-32- built-in aliases
// (Administrators, Users, ...), and the S-1-5-80- service range up front. The
// SidTypeUser check is kept only for a lookup that SUCCEEDS, where it still
// drops a group SID. A lookup that FAILS is admitted, not dropped: an empty
// system name falls through to the domain, so a deleted account and an
// unreachable DC are the same error, and dropping on it would turn a transient
// network fault into an operator lockout. The walk is bounded by the DACL's own uint16 ACE count;
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
		if ExcludedSID(s) {
			continue
		}
		// A failed lookup and a deleted account are the SAME signal here, and
		// the consequences are not symmetric: LookupAccount with an empty
		// system name falls through to the domain, so a DC blip on a
		// domain-joined host would drop a live operator's SID and lock them
		// out of their own daemon -- silently, on a transient network fault.
		// Admitting a SID that no longer resolves only shows a stale entry in
		// an operator listing. Keep the type filter where it can mean
		// something (a successful lookup naming a group), and admit the SID
		// where it cannot.
		switch _, _, accType, err := sid.LookupAccount(""); {
		case err != nil:
			slog.Warn("operator SID did not resolve; treating it as an operator rather than dropping it", "sid", s, "err", err)
		case accType != windows.SidTypeUser:
			continue
		}
		ids = append(ids, s)
	}
	return ids, nil
}

// ExcludedSID reports whether a SID string can never be an operator: the
// well-known singletons SYSTEM (S-1-5-18), LOCAL SERVICE (S-1-5-19), and
// NETWORK SERVICE (S-1-5-20); the S-1-5-32- built-in alias range
// (Administrators, Users, and the rest); and the S-1-5-80- service SID range
// (`NT SERVICE\*`, including the daemon's own account). Prefix-matched so it is
// independent of LookupAccountSid's type verdict, which misreports the service
// SID as a user in the daemon's context. Exported so the grant-target refusal
// (internal/socket) and this read-side exclusion enforce one identical rule —
// a SID refused for write must never be one that would be hidden from the
// read, or a live ACE could orphan out of ListIDs' view.
func ExcludedSID(sid string) bool {
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
