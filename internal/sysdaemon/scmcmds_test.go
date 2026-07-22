package sysdaemon

import (
	"reflect"
	"testing"
)

func TestWindowsServiceSID(t *testing.T) {
	if got, want := windowsServiceSID, `NT SERVICE\runnyd`; got != want {
		t.Errorf("windowsServiceSID = %q, want %q", got, want)
	}
}

func TestIcaclsHomeArgs(t *testing.T) {
	got := icaclsHomeArgs(`C:\ProgramData\runny`, `CORP\alice`)
	want := [][]string{
		{"icacls", `C:\ProgramData\runny`, "/setowner", `CORP\alice`, "/T"},
		{"icacls", `C:\ProgramData\runny`, "/inheritance:d", "/T"},
		{"icacls", `C:\ProgramData\runny`, "/remove:g", `BUILTIN\Users`, "/T"},
		{"icacls", `C:\ProgramData\runny`, "/grant", `NT SERVICE\runnyd:(OI)(CI)M`, "/T"},
		{"icacls", `C:\ProgramData\runny`, "/grant", `CORP\alice:(OI)(CI)M`, "/T"},
		{"icacls", `C:\ProgramData\runny`, "/setowner", `NT SERVICE\runnyd`, "/T"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("icaclsHomeArgs =\n got %v\nwant %v", got, want)
	}
}

// Regression test: /reset + /inheritance:r looked equivalent to /inheritance:d
// alone but wasn't -- it reproducibly left a directory with an empty DACL
// (denying even the elevated caller) once real hardware testing exercised a
// two-level tree. /inheritance:d must never appear paired with /reset again.
func TestIcaclsHomeArgsNeverPairsResetWithInheritanceR(t *testing.T) {
	for _, args := range icaclsHomeArgs(`C:\ProgramData\runny`, `CORP\alice`) {
		for _, a := range args {
			if a == "/reset" || a == "/inheritance:r" {
				t.Errorf("icaclsHomeArgs must not use %q (real-hardware-confirmed empty-DACL hazard on a two-level tree): %v", a, args)
			}
		}
	}
}
