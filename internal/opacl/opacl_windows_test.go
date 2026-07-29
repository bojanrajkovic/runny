//go:build windows

package opacl

import (
	"reflect"
	"slices"
	"testing"

	"golang.org/x/sys/windows"
)

// TestGrantRevokeArgs pins each verb to ONE icacls invocation against ONE
// object, with an INHERITING Modify entry — the opposite of darwin's, and
// correct for the opposite reason: windows keeps non-protected children in sync
// with the parent in both directions, so revokeArgs' single /remove:g against
// the home takes the entry off every descendant that inherited it. Darwin's
// copy-at-create inheritance cannot be undone that way, which is why its entry
// must not inherit.
func TestGrantRevokeArgs(t *testing.T) {
	got := grantArgs(`C:\ProgramData\runny`, `CORP\alice`)
	want := []string{"icacls", `C:\ProgramData\runny`, "/grant", `CORP\alice:(OI)(CI)M`}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("grantArgs =\n got %v\nwant %v", got, want)
	}
	got = revokeArgs(`C:\ProgramData\runny`, `CORP\alice`)
	want = []string{"icacls", `C:\ProgramData\runny`, "/remove:g", `CORP\alice`}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("revokeArgs =\n got %v\nwant %v", got, want)
	}
}

// TestOperatorACEShape pins the pure half of the membership rule against
// synthetic ACE data: allow-type, not inherit-only, and a mask carrying
// FILE_WRITE_DATA — each dimension flips membership independently.
func TestOperatorACEShape(t *testing.T) {
	const modify = windows.FILE_GENERIC_READ | windows.FILE_GENERIC_WRITE | windows.FILE_GENERIC_EXECUTE | windows.DELETE
	cases := []struct {
		name     string
		aceType  uint8
		aceFlags uint8
		mask     uint32
		want     bool
	}{
		{"modify allow ACE (the operator shape)", windows.ACCESS_ALLOWED_ACE_TYPE, 0x3 /* OI|CI */, modify, true},
		{"non-inheriting allow ACE (the socket stamp)", windows.ACCESS_ALLOWED_ACE_TYPE, 0, modify, true},
		{"deny ACE", windows.ACCESS_DENIED_ACE_TYPE, 0, modify, false},
		{"inherit-only ACE (CREATOR OWNER's shape)", windows.ACCESS_ALLOWED_ACE_TYPE, windows.INHERIT_ONLY_ACE | 0x3, modify, false},
		{"read-only allow ACE (no FILE_WRITE_DATA)", windows.ACCESS_ALLOWED_ACE_TYPE, 0, windows.FILE_GENERIC_READ, false},
	}
	for _, c := range cases {
		if got := operatorACEShape(c.aceType, c.aceFlags, c.mask); got != c.want {
			t.Errorf("%s: operatorACEShape = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestOperatorSIDsWalk drives the real DACL walk (GetAce, the SID cast, the
// exclusion, the SidTypeUser resolution) against a synthetic in-memory ACL:
// the current user's allow-write ACE is a member; SYSTEM's identical ACE is
// not (excluded structurally by its S-1-5-18 SID, the same exclusion that
// keeps the bootstrap's SYSTEM Full grant out of the operator list on a real
// home).
func TestOperatorSIDsWalk(t *testing.T) {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatalf("GetTokenUser: %v", err)
	}
	me := tu.User.Sid
	if _, _, accType, err := me.LookupAccount(""); err != nil || accType != windows.SidTypeUser {
		t.Skipf("current token user is not a plain user account (type %d, err %v)", accType, err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatalf("CreateWellKnownSid(SYSTEM): %v", err)
	}

	entry := func(sid *windows.SID) windows.EXPLICIT_ACCESS {
		return windows.EXPLICIT_ACCESS{
			AccessPermissions: windows.FILE_GENERIC_READ | windows.FILE_GENERIC_WRITE,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeValue: windows.TrusteeValueFromSID(sid),
			},
		}
	}
	dacl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{entry(me), entry(system)}, nil)
	if err != nil {
		t.Fatalf("ACLFromEntries: %v", err)
	}

	ids, err := operatorSIDs(dacl)
	if err != nil {
		t.Fatalf("operatorSIDs: %v", err)
	}
	if !slices.Contains(ids, me.String()) {
		t.Errorf("current user's allow-write ACE missing from %v", ids)
	}
	if slices.Contains(ids, system.String()) {
		t.Errorf("SYSTEM must be excluded (SidTypeWellKnownGroup), got %v", ids)
	}
}

// TestOperatorSIDsExcludesServiceSID is the red test for the service-SID leak:
// a service SID (S-1-5-80-*, e.g. NT SERVICE\runnyd) carrying an allow-write
// ACE — exactly the shape the install bootstrap writes for the daemon's own
// account, and exactly what LookupAccountSid mislabels as SidTypeUser in the
// daemon's context. The structural prefix exclusion must drop it regardless of
// that type verdict, so the daemon never counts itself as an operator.
func TestOperatorSIDsExcludesServiceSID(t *testing.T) {
	svc, err := windows.StringToSid("S-1-5-80-3139157870-2983391045-3678747466-658725712-1809340420")
	if err != nil {
		t.Fatalf("StringToSid: %v", err)
	}
	entry := windows.EXPLICIT_ACCESS{
		AccessPermissions: windows.FILE_GENERIC_READ | windows.FILE_GENERIC_WRITE,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeValue: windows.TrusteeValueFromSID(svc),
		},
	}
	dacl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{entry}, nil)
	if err != nil {
		t.Fatalf("ACLFromEntries: %v", err)
	}
	ids, err := operatorSIDs(dacl)
	if err != nil {
		t.Fatalf("operatorSIDs: %v", err)
	}
	if slices.Contains(ids, svc.String()) {
		t.Errorf("service SID (S-1-5-80-*) must be excluded structurally, got %v", ids)
	}
}

// TestExcludedSID pins the structural exclusion directly: the well-known
// singletons and the alias/service ranges are excluded; a real per-machine
// user SID (S-1-5-21-*) is not.
func TestExcludedSID(t *testing.T) {
	for _, sid := range []string{"S-1-5-18", "S-1-5-19", "S-1-5-20", "S-1-5-32-544", "S-1-5-80-0", "S-1-5-80-1-2-3-4-5"} {
		if !ExcludedSID(sid) {
			t.Errorf("ExcludedSID(%q) = false, want true", sid)
		}
	}
	for _, sid := range []string{"S-1-5-21-1111111111-2222222222-3333333333-1001", "S-1-5-21-9-9-9-500"} {
		if ExcludedSID(sid) {
			t.Errorf("ExcludedSID(%q) = true, want false", sid)
		}
	}
}

// TestOperatorSIDsNilDACL pins the no-DACL degenerate case: nothing
// enumerable, no error — mirroring darwin's no-ACL nil, nil.
func TestOperatorSIDsNilDACL(t *testing.T) {
	ids, err := operatorSIDs(nil)
	if err != nil || ids != nil {
		t.Errorf("operatorSIDs(nil) = %v, %v; want nil, nil", ids, err)
	}
}
