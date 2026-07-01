// Package tart implements runny's compatibility with tart's VM bundle format
// a directory of config.json + disk.img + nvram.bin. runny never
// invokes the tart binary; this package and internal/oci together replace it.
// Cilicon (MIT) is the reference implementation for the format.
package tart

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// BundleFiles are the three files that constitute a tart VM bundle. A clone
// is a copy-on-write clone of exactly these.
var BundleFiles = []string{"config.json", "disk.img", "nvram.bin"}

var (
	// ErrUnsupportedDiskFormat: ASIF (macOS 26 tart) is rejected until vz
	// attachment support is verified — a clear error beats a hung boot.
	ErrUnsupportedDiskFormat = errors.New("unsupported disk format (only raw is supported)")
	// ErrUnsupportedGuest rejects bundles outside darwin/linux on arm64.
	ErrUnsupportedGuest = errors.New("bundle is not a darwin/arm64 or linux/arm64 guest")
)

// Bundle is a tart-format VM bundle directory.
type Bundle string

func (b Bundle) ConfigPath() string { return filepath.Join(string(b), "config.json") }
func (b Bundle) DiskPath() string   { return filepath.Join(string(b), "disk.img") }
func (b Bundle) NVRAMPath() string  { return filepath.Join(string(b), "nvram.bin") }

// Config is tart's config.json. hardwareModel and ecid are base64-encoded
// Virtualization.framework data representations; this package keeps them as
// raw bytes and internal/vm turns them into VZ objects.
type Config struct {
	Version       int    `json:"version"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	CPUCount      uint   `json:"cpuCount"`
	CPUCountMin   uint   `json:"cpuCountMin"`
	MemorySize    uint64 `json:"memorySize"`
	MemorySizeMin uint64 `json:"memorySizeMin"`
	MACAddress    string `json:"macAddress"`
	DiskFormat    string `json:"diskFormat"`

	HardwareModelB64 string `json:"hardwareModel"`
	ECIDB64          string `json:"ecid"`

	Display struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	} `json:"display"`
}

// HardwareModel decodes the VZMacHardwareModel data representation. Empty
// decodes fail here: vz's *WithData constructors index &b[0] unguarded.
func (c *Config) HardwareModel() ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(c.HardwareModelB64)
	if err != nil {
		return nil, fmt.Errorf("decoding hardwareModel: %w", err)
	}
	if len(b) == 0 {
		return nil, errors.New("decoding hardwareModel: empty data representation")
	}
	return b, nil
}

// ECID decodes the VZMacMachineIdentifier data representation. Boots reuse
// this persisted identifier — a fresh one paired with the bundle's aux
// storage boots the guest on the image's baked, stale RTC, and GitHub
// rejects the runner's JIT token. Clones share it, as tart's clones do.
func (c *Config) ECID() ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(c.ECIDB64)
	if err != nil {
		return nil, fmt.Errorf("decoding ecid: %w", err)
	}
	if len(b) == 0 {
		return nil, errors.New("decoding ecid: empty data representation")
	}
	return b, nil
}

// LoadConfig reads and validates a bundle's config.json. darwin and linux
// arm64 guests are supported; the VZ data representations
// (hardwareModel/ecid) exist only on darwin bundles — linux boots via EFI
// with the nvram.bin file as its variable store.
func (b Bundle) LoadConfig() (*Config, error) {
	raw, err := os.ReadFile(b.ConfigPath())
	if err != nil {
		return nil, fmt.Errorf("reading bundle config: %w", err)
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", b.ConfigPath(), err)
	}
	if (c.OS != "darwin" && c.OS != "linux") || c.Arch != "arm64" {
		return nil, fmt.Errorf("%w: %s/%s", ErrUnsupportedGuest, c.OS, c.Arch)
	}
	if c.DiskFormat != "" && c.DiskFormat != "raw" {
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedDiskFormat, c.DiskFormat)
	}
	if c.OS == "darwin" && (c.HardwareModelB64 == "" || c.ECIDB64 == "") {
		return nil, errors.New("darwin bundle config missing hardwareModel or ecid")
	}
	if c.CPUCount == 0 || c.MemorySize == 0 {
		return nil, errors.New("bundle config missing cpuCount or memorySize")
	}
	return &c, nil
}

// Verify checks that all three bundle files exist and are non-empty.
func (b Bundle) Verify() error {
	for _, f := range BundleFiles {
		fi, err := os.Stat(filepath.Join(string(b), f))
		if err != nil {
			return fmt.Errorf("bundle missing %s: %w", f, err)
		}
		if fi.Size() == 0 {
			return fmt.Errorf("bundle file %s is empty", f)
		}
	}
	return nil
}
