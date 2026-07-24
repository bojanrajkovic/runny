package main

import (
	"fmt"
	"os"

	"github.com/bojanrajkovic/runny/internal/oci"
	"github.com/bojanrajkovic/runny/internal/tart"
)

// imagePlaceholderNVRAM is what a packed image's nvram layer holds when the
// operator doesn't supply real nvram bytes via --nvram. Safe ONLY for
// windows: HCS never reads NVRAMPath at all (see internal/vm/hcs_windows.go),
// so any non-empty stand-in satisfies tart.Bundle.Verify without mattering
// further. It is NOT safe for darwin/linux -- internal/vm's VZ backend opens
// and parses NVRAMPath as real EFI/aux-storage firmware state
// (vz.NewEFIVariableStore/vz.NewMacAuxiliaryStorage) -- so imagePack requires
// --nvram explicitly for those guests instead of silently reusing this.
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
	diskInfo, err := disk.Stat()
	if err != nil {
		return fmt.Errorf("stating %s: %w", diskPath, err)
	}

	nvram := imagePlaceholderNVRAM
	switch {
	case nvramPath != "":
		nvram, err = os.ReadFile(nvramPath)
		if err != nil {
			return fmt.Errorf("reading %s: %w", nvramPath, err)
		}
	case guestOS != "windows":
		return fmt.Errorf("--nvram is required for --os %s (only windows guests can use the placeholder -- HCS never reads it, VZ does)", guestOS)
	}

	// Version is left unset: nothing in runny reads tart.Config.Version (grepped),
	// so there's no real value to assign it and no reason to fabricate one.
	cfg := tart.Config{
		OS:         guestOS,
		Arch:       arch,
		CPUCount:   cpuCount,
		MemorySize: memorySize,
	}
	packed, err := oci.WriteImage(layoutDir, cfg, disk, diskInfo.Size(), nvram)
	if err != nil {
		return fmt.Errorf("packing %s: %w", diskPath, err)
	}
	fmt.Printf("packed %s\n  layout: %s\n  digest: %s\n", diskPath, packed.Dir, packed.Digest)
	return nil
}
