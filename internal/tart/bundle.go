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
	"io"
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

// DiskFormat values recognized in config.json's diskFormat field. An empty
// string is treated the same as DiskFormatRaw (older tart bundles omit the
// field entirely).
const (
	DiskFormatRaw  = "raw"
	DiskFormatASIF = "asif"
)

var (
	// ErrUnsupportedDiskFormat rejects any diskFormat value this package
	// doesn't know how to boot. "raw" (or an empty field, from older
	// bundles) is a plain disk image; "asif" is tart's macOS-26 sparse image
	// format, accepted here as a shape only — whether THIS host can actually
	// attach one is a separate, host-relative check (see LoadConfig's own
	// doc comment for why that split exists).
	ErrUnsupportedDiskFormat = errors.New("unsupported disk format (only raw and asif are supported)")
	// ErrUnsupportedGuest rejects any (OS, Arch) shape outside darwin/arm64,
	// linux/{arm64,amd64}, or windows/{arm64,amd64} — see LoadConfig.
	ErrUnsupportedGuest = errors.New("bundle is not a darwin/arm64, linux/arm64, linux/amd64, windows/arm64, or windows/amd64 guest")
	// ErrASIFUnsupportedOnHost is wrapped by every host-capability refusal of
	// an ASIF disk: the darwin version gate (internal/vm) and Hyper-V's
	// outright refusal (internal/oci, internal/images).
	ErrASIFUnsupportedOnHost = errors.New("this host cannot boot ASIF disks")
	// ErrASIFPackUnsupported: runnyctl image pack only emits raw or VHDX
	// disks, and relabeling ASIF bytes as raw would ship an unbootable image.
	ErrASIFPackUnsupported = errors.New("image pack does not support ASIF disks; convert the disk to raw first")
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

// IsASIF reports whether the bundle's disk is tart's ASIF format rather than
// a plain raw image, so callers don't need to string-compare DiskFormat
// themselves.
func (c *Config) IsASIF() bool { return c.DiskFormat == DiskFormatASIF }

// CheckDiskFormat returns ErrUnsupportedDiskFormat unless DiskFormat is empty,
// raw, or asif. The puller runs it before the disk download, so an image in
// a format this package can't boot costs only its config blob.
func (c *Config) CheckDiskFormat() error {
	switch c.DiskFormat {
	case "", DiskFormatRaw, DiskFormatASIF:
		return nil
	}
	return fmt.Errorf("%w: %q", ErrUnsupportedDiskFormat, c.DiskFormat)
}

// RefuseASIFOnHyperV rejects c if its disk is labeled ASIF: Hyper-V has no
// attach path for ASIF on any host version.
func (c *Config) RefuseASIFOnHyperV() error {
	if c.IsASIF() {
		return fmt.Errorf("%w: Hyper-V has no ASIF support; use an image with a raw disk", ErrASIFUnsupportedOnHost)
	}
	return nil
}

// asifMagic is the 4-byte signature at the start of an Apple Sparse Image
// Format file ("shdw"), confirmed against `diskutil image create blank
// --format ASIF` output on a macOS 26+ host.
var asifMagic = [4]byte{'s', 'h', 'd', 'w'}

// IsASIFDisk sniffs r's first 4 bytes for the ASIF magic, rather than
// trusting a bundle's diskFormat label — which can be wrong (see
// internal/images' prepareBundleDisk, which checks both the label and the
// bytes). A file shorter than 4 bytes simply isn't ASIF (false, nil), not an
// error — there's nothing to identify, and a truncated disk is a Verify
// concern, not this function's.
func IsASIFDisk(r io.ReaderAt) (bool, error) {
	var buf [4]byte
	n, err := r.ReadAt(buf[:], 0)
	if n < len(buf) {
		if err != nil && err != io.EOF {
			return false, fmt.Errorf("reading disk magic: %w", err)
		}
		return false, nil
	}
	return buf == asifMagic, nil
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
	if err := c.CheckDiskFormat(); err != nil {
		return nil, err
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
