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

// CompatVersion is the tart release whose bundle/OCI format this package
// tracks, advertised to guests via GuestAgentPortName.
const CompatVersion = "2.32.1"

// GuestAgentPortName is the console-port name the cirruslabs images'
// tart-guest-agent requires ("tart-version-" + digits-only semver, major
// ≥ 2). Load-bearing: without it the agent repeatedly kills launchd and
// macOS guests are unusable.
const GuestAgentPortName = "tart-version-" + CompatVersion

var (
	// ErrUnsupportedDiskFormat: ASIF (macOS 26 tart) is rejected until vz
	// attachment support is verified — a clear error beats a hung boot.
	ErrUnsupportedDiskFormat = errors.New("unsupported disk format (only raw is supported)")
	// ErrUnsupportedGuest rejects any (OS, Arch) shape outside darwin/arm64,
	// linux/{arm64,amd64}, or windows/{arm64,amd64} — see LoadConfig.
	ErrUnsupportedGuest = errors.New("bundle is not a darwin/arm64, linux/arm64, linux/amd64, windows/arm64, or windows/amd64 guest")
)

// Bundle is a tart-format VM bundle directory.
type Bundle string

func (b Bundle) ConfigPath() string { return filepath.Join(string(b), "config.json") }
func (b Bundle) DiskPath() string   { return filepath.Join(string(b), "disk.img") }
func (b Bundle) NVRAMPath() string  { return filepath.Join(string(b), "nvram.bin") }

// VHDXPath is the Hyper-V backend's converted disk (internal/images'
// post-pull conversion via internal/vhdx.Convert). Every pull produces
// DiskPath regardless of host; VHDXPath exists only once a windows host has
// converted it, and DiskPath is removed once VHDXPath exists (see
// prepareBundleDisk) — Verify accepts either.
func (b Bundle) VHDXPath() string { return filepath.Join(string(b), "disk.vhdx") }

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

// LoadConfig reads and validates a bundle's config.json. This is a pure
// shape check — "is this a bundle my code knows how to interpret at all" —
// deliberately independent of the host it happens to run on, so it stays
// portable/testable everywhere. darwin/arm64 boots via VZ's Mac platform;
// linux/{arm64,amd64} and windows/{arm64,amd64} all boot via EFI, VZ on
// darwin/arm64 hosts or HCS on Windows hosts respectively. Neither
// Virtualization.framework nor Hyper-V cross-emulates architectures (Rosetta
// translates userspace binaries inside an already-booted arm64 Linux guest,
// it does not let VZ boot an amd64 kernel), so "can THIS host actually boot
// THIS arch" is a separate, host-capability check each platform's own
// Manager.Boot makes against its own runtime.GOARCH — not something this
// portable check can know. windows/arm64 is accepted for the same reason
// linux/arm64 is: an arm64 Windows host running Hyper-V is a real (if
// currently unvalidated) target, and hardcoding Windows guests to amd64 here
// would be a second, needless host-arch opinion this check
// deliberately doesn't hold for any other OS.
func (b Bundle) LoadConfig() (*Config, error) {
	raw, err := os.ReadFile(b.ConfigPath())
	if err != nil {
		return nil, fmt.Errorf("reading bundle config: %w", err)
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", b.ConfigPath(), err)
	}
	switch {
	case c.OS == "darwin" && c.Arch == "arm64":
	case c.OS == "linux" && (c.Arch == "arm64" || c.Arch == "amd64"):
	case c.OS == "windows" && (c.Arch == "arm64" || c.Arch == "amd64"):
	default:
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

// Verify checks that config.json and nvram.bin exist and are non-empty, and
// that the disk is present in whichever form this bundle currently keeps it
// in — DiskPath (every fresh pull) or VHDXPath (windows, once converted and
// DiskPath removed; see prepareBundleDisk). Accepting either, rather than
// requiring DiskPath specifically, is what lets a windows bundle reclaim the
// raw copy's disk space without this cache-hit check forcing a full re-pull
// on every subsequent Ensure.
func (b Bundle) Verify() error {
	for _, f := range []string{"config.json", "nvram.bin"} {
		if err := verifyNonEmpty(filepath.Join(string(b), f)); err != nil {
			return err
		}
	}
	diskErr := verifyNonEmpty(b.DiskPath())
	vhdxErr := verifyNonEmpty(b.VHDXPath())
	if diskErr != nil && vhdxErr != nil {
		return fmt.Errorf("bundle has neither disk.img nor disk.vhdx: %w", diskErr)
	}
	return nil
}

func verifyNonEmpty(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("bundle missing %s: %w", filepath.Base(path), err)
	}
	if fi.Size() == 0 {
		return fmt.Errorf("bundle file %s is empty", filepath.Base(path))
	}
	return nil
}
