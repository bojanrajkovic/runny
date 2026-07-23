//go:build windows

package opacl

import (
	"reflect"
	"slices"
	"testing"

	"golang.org/x/sys/windows"
)

// TestGrantRevokeArgs pins the exact icacls invocations: the home ACE must
// be byte-identical to the install bootstrap's operator grant ((OI)(CI)M —
// see internal/sysdaemon's icaclsHomeArgs) so ListIDs cannot tell a
// bootstrap operator from a live-granted one, and revoke must hit the home
// dir first (the ACL the revocation gate reads).
func TestGrantRevokeArgs(t *testing.T) {
	got := grantArgs(`C:\ProgramData\runny`, `C:\ProgramData\runny\runnyd.sock`, `CORP\alice`)
	want := [][]string{
		{"icacls", `C:\ProgramData\runny`, "/grant", `CORP\alice:(OI)(CI)M`},
		{"icacls", `C:\ProgramData\runny\runnyd.sock`, "/grant", `CORP\alice:M`},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("grantArgs =\n got %v\nwant %v", got, want)
	}

	got = revokeArgs(`C:\ProgramData\runny`, `C:\ProgramData\runny\runnyd.sock`, `CORP\alice`)
	want = [][]string{
		{"icacls", `C:\ProgramData\runny`, "/remove:g", `CORP\alice`},
		{"icacls", `C:\ProgramData\runny\runnyd.sock`, "/remove:g", `CORP\alice`},
	}
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
// SidTypeUser resolution) against a synthetic in-memory ACL: the current
// user's allow-write ACE is a member; SYSTEM's identical ACE is not
// (SidTypeWellKnownGroup, the same exclusion that keeps the bootstrap's
// SYSTEM Full grant out of the operator list on a real home).
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

// TestOperatorSIDsNilDACL pins the no-DACL degenerate case: nothing
// enumerable, no error — mirroring darwin's no-ACL nil, nil.
func TestOperatorSIDsNilDACL(t *testing.T) {
	ids, err := operatorSIDs(nil)
	if err != nil || ids != nil {
		t.Errorf("operatorSIDs(nil) = %v, %v; want nil, nil", ids, err)
	}
}
