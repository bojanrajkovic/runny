package tart

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The agent accepts only "tart-version-" + a digits-only MAJOR.MINOR.PATCH
// (no signs, no leading zeros), major ≥ 2. Pin the advertised name.
func TestGuestAgentPortNameSatisfiesContract(t *testing.T) {
	version, ok := strings.CutPrefix(GuestAgentPortName, "tart-version-")
	if !ok {
		t.Fatalf("GuestAgentPortName %q must start with tart-version-", GuestAgentPortName)
	}
	if version != CompatVersion {
		t.Fatalf("GuestAgentPortName %q must advertise CompatVersion %q", GuestAgentPortName, CompatVersion)
	}
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		t.Fatalf("CompatVersion %q is not MAJOR.MINOR.PATCH", CompatVersion)
	}
	for _, p := range parts {
		if p == "" {
			t.Fatalf("CompatVersion %q has an empty component", CompatVersion)
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				t.Fatalf("CompatVersion %q has a non-digit character in component %q", CompatVersion, p)
			}
		}
		if len(p) > 1 && p[0] == '0' {
			t.Fatalf("CompatVersion %q has a leading zero in component %q", CompatVersion, p)
		}
	}
	if parts[0] == "0" || parts[0] == "1" {
		t.Fatalf("CompatVersion %q has major < 2 — the guest agent ignores it", CompatVersion)
	}
}

// realConfig is the literal config.json shape observed on a live tart 2.32
// bundle (runner-1 on ix, 2026-06-09), values intact.
const realConfig = `{
  "version" : 1,
  "hardwareModel" : "YnBsaXN0MDDTAQIDBAQFXxAZRGF0YVJlcHJlc2VudGF0aW9uVmVyc2lvbl8QD1BsYXRmb3JtVmVyc2lvbl8QEk1pbmltdW1TdXBwb3J0ZWRPUxACowYHBxANEAAIDys9UlRYWgAAAAAAAAEBAAAAAAAAAAgAAAAAAAAAAAAAAAAAAABc",
  "ecid" : "YnBsaXN0MDDRAQJURUNJRBQAAAAAAAAAAMPnGW1rnZiDCAsQAAAAAAAAAQEAAAAAAAAAAwAAAAAAAAAAAAAAAAAAACE=",
  "cpuCountMin" : 2,
  "arch" : "arm64",
  "os" : "darwin",
  "cpuCount" : 2,
  "memorySizeMin" : 4294967296,
  "memorySize" : 4294967296,
  "display" : {
    "width" : 1024,
    "height" : 768
  },
  "diskFormat" : "raw",
  "macAddress" : "7a:47:bb:5f:97:c9"
}`

func writeBundle(t *testing.T, config string) Bundle {
	t.Helper()
	dir := t.TempDir()
	for f, body := range map[string]string{
		"config.json": config,
		"disk.img":    "not-a-real-disk",
		"nvram.bin":   "not-real-nvram",
	} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return Bundle(dir)
}

func TestLoadConfigReal(t *testing.T) {
	c, err := writeBundle(t, realConfig).LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if c.CPUCount != 2 || c.MemorySize != 4294967296 {
		t.Errorf("cpu/mem = %d/%d", c.CPUCount, c.MemorySize)
	}
	if c.MACAddress != "7a:47:bb:5f:97:c9" {
		t.Errorf("mac = %q", c.MACAddress)
	}
	hw, err := c.HardwareModel()
	if err != nil || len(hw) == 0 {
		t.Errorf("HardwareModel: %d bytes, %v", len(hw), err)
	}
	// VZ data representations are binary plists.
	if string(hw[:6]) != "bplist" {
		t.Errorf("hardwareModel does not decode to a bplist: %q", hw[:6])
	}
	ecid, err := c.ECID()
	if err != nil || string(ecid[:6]) != "bplist" {
		t.Errorf("ECID decode: %q, %v", ecid[:6], err)
	}
}

func TestLoadConfigWindowsGuest(t *testing.T) {
	// Windows bundles carry no hardwareModel/ecid either — they boot via EFI,
	// same as linux (see TestLoadConfigLinuxGuest).
	b := writeBundle(t, `{"version":1,"os":"windows","arch":"amd64","cpuCount":2,"memorySize":4294967296,
		"macAddress":"7a:47:bb:5f:97:c9","diskFormat":"raw"}`)
	c, err := b.LoadConfig()
	if err != nil {
		t.Fatalf("windows bundle: %v", err)
	}
	if c.OS != "windows" || c.Arch != "amd64" {
		t.Errorf("config = %+v", c)
	}
}

func TestLoadConfigRejectsASIF(t *testing.T) {
	b := writeBundle(t, `{"version":1,"os":"darwin","arch":"arm64","cpuCount":2,"memorySize":1,
		"hardwareModel":"eA==","ecid":"eA==","diskFormat":"asif"}`)
	_, err := b.LoadConfig()
	if !errors.Is(err, ErrUnsupportedDiskFormat) {
		t.Errorf("want ErrUnsupportedDiskFormat, got %v", err)
	}
}

func TestLoadConfigLinuxGuest(t *testing.T) {
	// Linux bundles carry no hardwareModel/ecid — they boot via EFI.
	b := writeBundle(t, `{"version":1,"os":"linux","arch":"arm64","cpuCount":4,"memorySize":4294967296,
		"macAddress":"7a:47:bb:5f:97:c9","diskFormat":"raw"}`)
	c, err := b.LoadConfig()
	if err != nil {
		t.Fatalf("linux bundle: %v", err)
	}
	if c.OS != "linux" || c.CPUCount != 4 {
		t.Errorf("config = %+v", c)
	}
}

func TestLoadConfigRejectsUnsupportedGuests(t *testing.T) {
	for name, cfg := range map[string]string{
		"windows/arm64": `{"version":1,"os":"windows","arch":"arm64","cpuCount":2,"memorySize":1}`,
		"darwin/amd64":  `{"version":1,"os":"darwin","arch":"amd64","cpuCount":2,"memorySize":1}`,
		"unknown arch":  `{"version":1,"os":"linux","arch":"riscv64","cpuCount":2,"memorySize":1}`,
	} {
		_, err := writeBundle(t, cfg).LoadConfig()
		if !errors.Is(err, ErrUnsupportedGuest) {
			t.Errorf("%s: want ErrUnsupportedGuest, got %v", name, err)
		}
	}
	// darwin without the VZ data representations is broken, not just odd.
	_, err := writeBundle(t, `{"version":1,"os":"darwin","arch":"arm64","cpuCount":2,"memorySize":1}`).LoadConfig()
	if err == nil {
		t.Error("darwin bundle without hardwareModel/ecid should fail")
	}
}

// LoadConfig is a pure shape check, independent of the host it runs on: it
// accepts every (OS, Arch) combination some platform's Manager.Boot can
// actually drive, even ones this host can't itself boot right now (a linux
// CI runner still parses a linux/amd64 config) — the "can THIS host boot
// THIS arch" gate is each platform's own Boot, not this check (see
// hcs_windows.go/vz_darwin.go's runtime.GOARCH rejection).
func TestLoadConfigAcceptsEveryBootableGuestShape(t *testing.T) {
	for name, cfg := range map[string]string{
		"linux/arm64":   `{"version":1,"os":"linux","arch":"arm64","cpuCount":2,"memorySize":1}`,
		"linux/amd64":   `{"version":1,"os":"linux","arch":"amd64","cpuCount":2,"memorySize":1}`,
		"windows/amd64": `{"version":1,"os":"windows","arch":"amd64","cpuCount":2,"memorySize":1}`,
	} {
		if _, err := writeBundle(t, cfg).LoadConfig(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestVerify(t *testing.T) {
	b := writeBundle(t, realConfig)
	if err := b.Verify(); err != nil {
		t.Errorf("Verify on complete bundle: %v", err)
	}
	if err := os.Remove(b.NVRAMPath()); err != nil {
		t.Fatal(err)
	}
	if err := b.Verify(); err == nil {
		t.Error("Verify should fail with nvram.bin missing")
	}
}

// A windows bundle deletes DiskPath once VHDXPath exists (prepareBundleDisk,
// internal/images) to reclaim the raw copy's disk space; Verify must still
// see that as complete, not force a re-pull.
func TestVerifyAcceptsVHDXInPlaceOfDiskImg(t *testing.T) {
	b := writeBundle(t, realConfig)
	if err := os.Remove(b.DiskPath()); err != nil {
		t.Fatal(err)
	}
	if err := b.Verify(); err == nil {
		t.Error("Verify should fail with neither disk.img nor disk.vhdx present")
	}
	if err := os.WriteFile(b.VHDXPath(), []byte("vhdx"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := b.Verify(); err != nil {
		t.Errorf("Verify should accept disk.vhdx in place of disk.img: %v", err)
	}
}
