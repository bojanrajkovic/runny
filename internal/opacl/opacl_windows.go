//go:build windows

package opacl

import (
	"fmt"
	"os/exec"
	"path/filepath"
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
	// Home LAST: it is the object the revocation gate reads, so it is what
	// makes someone an operator. If the logs\ command fails, nobody was
	// granted anything and the reported failure is the truth. Granting the
	// home first would leave a caller HasID authorizes but no audit record
	// names, and a retry that refuses because they are already an operator.
	return reverse(aclTargets(homeDir, "/grant", account+":(OI)(CI)M"))
}

func revokeArgs(homeDir, account string) [][]string {
	// Home FIRST, the mirror of grantArgs: revoking the authoritative object
	// first means a later failure leaves someone already denied by the gate
	// rather than still authorized. Both orders put the authoritative change
	// on the side where a partial run is safe.
	return aclTargets(homeDir, "/remove:g", account)
}

// reverse orders a command list authoritative-last.
func reverse(cmds [][]string) [][]string {
	out := make([][]string, 0, len(cmds))
	for i := len(cmds) - 1; i >= 0; i-- {
		out = append(out, cmds[i])
	}
	return out
}

// aclTargets names the two objects a fresh install gives an explicit operator
// ACE, home first; callers order it for their own partial-failure safety.
//
// KNOWN GAPS, both closing with the same change and neither papered over here.
//
// install-daemon re-run over a POPULATED home stamps explicit ACEs onto every
// existing descendant (icaclsHomeArgs carries /T on the grants), so on such a
// host a revoke reports success while leaving access on images/, vms/ and
// cycles/.
//
// And two commands can always half-run, in both directions. A grant whose home
// command fails leaves the target holding logs\ Modify, unaudited, while the
// caller is told the grant failed. A revoke whose logs\ command fails leaves
// that same ACE behind AND unremovable: membership is read from the home DACL,
// which is already gone, so every retry answers "is not an operator".
//
// Ordering buys the safe direction on each verb -- a failed grant authorizes
// nobody, a failed revoke denies immediately -- and cannot buy both properties
// at once, because the object that decides membership is also the object whose
// removal ends the ability to act. Neither compensating rollback nor a
// residual-tolerant precheck is added: each is a new failure path that exists
// only to serve this shape. Making home the single ACL authority collapses
// both verbs to ONE command, at which point no partial state exists to
// handle.
//
// The install bootstrap creates home and logs\ and then runs
// icacls /inheritance:d /T, which COPIES the then-inherited ACEs onto both and
// stops future propagation. logs\ therefore holds a real ACE of its own, not a
// reflection of the home's: a revoke of the home alone leaves a revoked
// operator with Modify on the daemon's logs, and a live grant of the home
// alone never reaches them at all. Everything created later (images/, vms/,
// cycles/) inherits normally and can never hold an explicit ACE.
//
// Deliberately NOT icacls /T. Without /C it aborts on the first object it
// cannot open for WRITE_DAC, and the daemon's own (OI)(CI)M does not include
// WRITE_DAC -- so a tree walk dies on any file the operator wrote (the App
// key, the atomically renamed config.yaml) AFTER removing the home's ACE,
// reporting failure for a revoke that partly happened and leaving it
// unrecorded. It would also descend multi-GB VHDX clones under a bound sized
// for a single directory.
func aclTargets(homeDir, verb, spec string) [][]string {
	return [][]string{
		{"icacls", homeDir, verb, spec},
		{"icacls", filepath.Join(homeDir, "logs"), verb, spec},
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
// SidTypeUser check is kept as a secondary filter: it still drops a SID that no
// longer resolves at all (a deleted account), which the grant/revoke path
// cannot act on anyway -- and which must not inflate the count RevokeOperator's
// last-operator guard reads. HasID, not this, answers "is this caller an
// operator": that question needs no name lookup and must not fail closed on a
// domain controller blip. The walk is bounded by the DACL's own uint16 ACE count;
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
		if _, _, accType, err := sid.LookupAccount(""); err != nil || accType != windows.SidTypeUser {
			continue
		}
		ids = append(ids, s)
	}
	return ids, nil
}

// HasID reports whether id holds an operator ACE on homeDir. It is the
// membership question the per-RPC revocation gate asks, and it deliberately
// does NOT go through operatorSIDs' name lookup: the caller's identity is
// already known, so resolving it buys nothing, while LookupAccount with an
// empty system name falls through to the domain -- making a DC blip on a
// domain-joined host lock a live operator out of their own daemon on every
// RPC. The listing path keeps the lookup because a name is what it exists to
// produce, and because a SID that no longer resolves must not inflate the
// count the last-operator guard reads.
func HasID(homeDir, id string) (bool, error) {
	sd, err := windows.GetNamedSecurityInfo(homeDir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false, fmt.Errorf("reading %s's security descriptor: %w", homeDir, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return false, fmt.Errorf("reading %s's DACL: %w", homeDir, err)
	}
	if dacl == nil {
		return false, nil
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return false, fmt.Errorf("reading ACE %d: %w", i, err)
		}
		if !operatorACEShape(ace.Header.AceType, ace.Header.AceFlags, uint32(ace.Mask)) {
			continue
		}
		s := (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()
		if !ExcludedSID(s) && s == id {
			return true, nil
		}
	}
	return false, nil
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
