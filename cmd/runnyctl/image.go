package main

import (
	"fmt"
	"os"

	"github.com/bojanrajkovic/runny/internal/oci"
	"github.com/bojanrajkovic/runny/internal/tart"
)

// imagePlaceholderNVRAM is what a packed image's nvram layer holds when the
// operator doesn't supply real nvram bytes via --nvram. HCS never reads
// NVRAMPath (see internal/vm/hcs_windows.go), but tart.Bundle.Verify still
// requires the file to be non-empty, so this is a Verify-satisfying stand-in,
// not meaningful VM firmware state.
var imagePlaceholderNVRAM = []byte{0}

// imagePack reads diskPath and packs it into a tart-format OCI Image Layout
// at layoutDir, entirely offline -- no registry, no daemon. It is a
// privileged-adjacent local command in the same sense install-daemon is: it
// must work on a host that has never run (or installed) runnyd at all, e.g.
// a Packer build machine, so its arguments arrive already parsed by kong
// (see ImagePackCmd) and it never touches a *ctl.
func imagePack(diskPath, layoutDir, guestOS, arch string, cpuCount uint, memorySize uint64, nvramPath string) error {
	disk, err := os.Open(diskPath)
	if err != nil {
		return fmt.Errorf("opening %s: %w", diskPath, err)
	}
	defer disk.Close()

	nvram := imagePlaceholderNVRAM
	if nvramPath != "" {
		nvram, err = os.ReadFile(nvramPath)
		if err != nil {
			return fmt.Errorf("reading %s: %w", nvramPath, err)
		}
	}

	// Version is left unset: nothing in runny reads tart.Config.Version (grepped),
	// so there's no real value to assign it and no reason to fabricate one.
	cfg := tart.Config{
		OS:         guestOS,
		Arch:       arch,
		CPUCount:   cpuCount,
		MemorySize: memorySize,
	}
	packed, err := oci.WriteImage(layoutDir, cfg, disk, nvram)
	if err != nil {
		return fmt.Errorf("packing %s: %w", diskPath, err)
	}
	fmt.Printf("packed %s\n  layout: %s\n  digest: %s\n", diskPath, packed.Dir, packed.Digest)
	return nil
}
