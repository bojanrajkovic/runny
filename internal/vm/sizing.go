package vm

import (
	"fmt"

	"github.com/bojanrajkovic/runny/internal/tart"
)

// resolveSizing applies the pool's CPU/RAM overrides over the bundle's baked
// values (BootOptions zero = keep the image's). A request below the bundle's
// recorded minimum is rejected with an actionable error rather than left to
// surface as VZ's generic config-validate failure (silent-failure-proofness).
// Deliberately untagged: the logic is platform-independent and this is the
// only way the Linux CI leg exercises it.
func resolveSizing(cfg *tart.Config, opts BootOptions) (uint, uint64, error) {
	cpu, mem := cfg.CPUCount, cfg.MemorySize
	if opts.CPUCount != 0 {
		if cfg.CPUCountMin != 0 && opts.CPUCount < cfg.CPUCountMin {
			return 0, 0, fmt.Errorf("cpu_cores=%d is below the image minimum of %d", opts.CPUCount, cfg.CPUCountMin)
		}
		cpu = opts.CPUCount
	}
	if opts.MemorySize != 0 {
		if cfg.MemorySizeMin != 0 && opts.MemorySize < cfg.MemorySizeMin {
			return 0, 0, fmt.Errorf("ram_gb (%d bytes) is below the image minimum of %d bytes", opts.MemorySize, cfg.MemorySizeMin)
		}
		mem = opts.MemorySize
	}
	return cpu, mem, nil
}
