package sysdaemon

import "testing"

func TestWindowsServiceSID(t *testing.T) {
	if got, want := windowsServiceSID(), `NT SERVICE\runnyd`; got != want {
		t.Errorf("windowsServiceSID() = %q, want %q", got, want)
	}
}

func TestIcaclsHomeArgs(t *testing.T) {
	got := icaclsHomeArgs(`C:\ProgramData\runny`, `CORP\alice`)
	want := [][]string{
		{"icacls", `C:\ProgramData\runny`, "/inheritance:r"},
		{"icacls", `C:\ProgramData\runny`, "/grant", `NT SERVICE\runnyd:(OI)(CI)M`, "/T"},
		{"icacls", `C:\ProgramData\runny`, "/grant", `CORP\alice:(OI)(CI)M`, "/T"},
		{"icacls", `C:\ProgramData\runny`, "/setowner", `NT SERVICE\runnyd`, "/T"},
	}
	if len(got) != len(want) {
		t.Fatalf("icaclsHomeArgs returned %d steps, want %d:\n got %v\nwant %v", len(got), len(want), got, want)
	}
	for i := range want {
		if len(got[i]) != len(want[i]) {
			t.Errorf("step %d = %v, want %v", i, got[i], want[i])
			continue
		}
		for j := range want[i] {
			if got[i][j] != want[i][j] {
				t.Errorf("step %d = %v, want %v", i, got[i], want[i])
				break
			}
		}
	}
}
