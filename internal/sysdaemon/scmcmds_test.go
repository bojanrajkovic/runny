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

// The home is the only object written to, and the children's /reset runs LAST
// so they inherit an ALREADY-clean home — running it earlier would leave the
// App key briefly readable by the ProgramData leak group. Only the two
// ownership steps carry /T, because ownership is not inherited.
func TestIcaclsHomeArgs(t *testing.T) {
	got := icaclsHomeArgs(`C:\ProgramData\runny`, `CORP\alice`)
	want := [][]string{
		{"icacls", `C:\ProgramData\runny`, "/setowner", `CORP\alice`, "/T"},
		{"icacls", `C:\ProgramData\runny`, "/inheritance:d"},
		{"icacls", `C:\ProgramData\runny`, "/remove:g", `*S-1-5-32-545`},
		{"icacls", `C:\ProgramData\runny`, "/grant", `NT SERVICE\runnyd:(OI)(CI)M`},
		{"icacls", `C:\ProgramData\runny`, "/grant", `CORP\alice:(OI)(CI)M`},
		{"icacls", `C:\ProgramData\runny\*`, "/reset", "/T"},
		{"icacls", `C:\ProgramData\runny`, "/setowner", `NT SERVICE\runnyd`, "/T"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("icaclsHomeArgs =\n got %v\nwant %v", got, want)
	}
}
