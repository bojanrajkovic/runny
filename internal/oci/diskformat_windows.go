//go:build windows

package oci

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/bojanrajkovic/runny/internal/tart"
)

// checkHostDiskFormat refuses an ASIF-labeled image as early in pull as the
// label is knowable -- right after config.json lands, before nvram.bin or
// any (potentially 80GB+) disk.v2 layer is downloaded. Hyper-V has no attach
// path for ASIF at all, so pulling the rest just to throw it away on the
// next Ensure would otherwise repeat every cycle (internal/images'
// prepareBundleDisk RemoveAlls the whole bundle on its own later refusal).
//
// This parses only the diskFormat field directly rather than going through
// tart.Bundle.LoadConfig, which also demands a valid arch/hardwareModel
// shape a config.json this package's own tests feed were never meant to
// satisfy, and which pull has no need for here.
func checkHostDiskFormat(configPath string) error {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", configPath, err)
	}
	var cfg tart.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("parsing %s: %w", configPath, err)
	}
	return cfg.RefuseASIFOnHyperV()
}
