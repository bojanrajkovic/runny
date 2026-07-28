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
		{"icacls", `C:\ProgramData\runny`, "/remove:g", `*S-1-5-32-545`, "/T"},
		{"icacls", `C:\ProgramData\runny`, "/grant", `NT SERVICE\runnyd:(OI)(CI)M`},
		{"icacls", `C:\ProgramData\runny`, "/grant", `CORP\alice:M`},
		{"icacls", `C:\ProgramData\runny`, "/setowner", `NT SERVICE\runnyd`, "/T"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("icaclsHomeArgs =\n got %v\nwant %v", got, want)
	}
}
