package tart

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

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

func TestLoadConfigRejectsASIF(t *testing.T) {
	b := writeBundle(t, `{"version":1,"os":"darwin","arch":"arm64","cpuCount":2,"memorySize":1,
		"hardwareModel":"eA==","ecid":"eA==","diskFormat":"asif"}`)
	_, err := b.LoadConfig()
	if !errors.Is(err, ErrUnsupportedDiskFormat) {
		t.Errorf("want ErrUnsupportedDiskFormat, got %v", err)
	}
}

func TestLoadConfigRejectsLinuxGuest(t *testing.T) {
	b := writeBundle(t, `{"version":1,"os":"linux","arch":"arm64","cpuCount":2,"memorySize":1,
		"hardwareModel":"eA==","ecid":"eA=="}`)
	_, err := b.LoadConfig()
	if !errors.Is(err, ErrNotMacOSGuest) {
		t.Errorf("want ErrNotMacOSGuest, got %v", err)
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
