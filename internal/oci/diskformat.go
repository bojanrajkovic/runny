package oci

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/bojanrajkovic/runny/internal/tart"
)

// checkConfigDiskFormat refuses an image whose diskFormat this host can't
// boot, right after config.json lands and before nvram.bin or any disk.v2
// layer downloads. Refusing later wastes the 80GB+ download, and on windows
// it repeats every cycle: internal/images' prepareBundleDisk refusal wipes
// the bundle, so the next Ensure pulls it again.
//
// It decodes only the fields it needs rather than calling
// tart.Bundle.LoadConfig, whose full shape check (arch, hardwareModel, sizes)
// the pull doesn't need and this package's test configs don't satisfy.
func checkConfigDiskFormat(configPath string) error {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", configPath, err)
	}
	var cfg tart.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("parsing %s: %w", configPath, err)
	}
	if err := cfg.CheckDiskFormat(); err != nil {
		return err
	}
	return refuseHostDiskFormat(&cfg)
}
