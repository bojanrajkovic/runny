//go:build darwin

package vm

import (
	"testing"

	"github.com/bojanrajkovic/runny/internal/tart"
)

func TestResolveSizing(t *testing.T) {
	// An image that bakes 2c/4GiB with a recorded floor of 2c/4GiB.
	const gib = uint64(1) << 30
	img := &tart.Config{CPUCount: 2, CPUCountMin: 2, MemorySize: 4 * gib, MemorySizeMin: 4 * gib}

	t.Run("zero keeps the image values", func(t *testing.T) {
		cpu, mem, err := resolveSizing(img, BootOptions{})
		if err != nil || cpu != 2 || mem != 4*gib {
			t.Fatalf("cpu=%d mem=%d err=%v", cpu, mem, err)
		}
	})

	t.Run("override above the minimum applies", func(t *testing.T) {
		cpu, mem, err := resolveSizing(img, BootOptions{CPUCount: 6, MemorySize: 12 * gib})
		if err != nil || cpu != 6 || mem != 12*gib {
			t.Fatalf("cpu=%d mem=%d err=%v", cpu, mem, err)
		}
	})

	t.Run("cpu below the image minimum is rejected", func(t *testing.T) {
		if _, _, err := resolveSizing(img, BootOptions{CPUCount: 1}); err == nil {
			t.Fatal("want error for cpu below minimum")
		}
	})

	t.Run("ram below the image minimum is rejected", func(t *testing.T) {
		if _, _, err := resolveSizing(img, BootOptions{MemorySize: 2 * gib}); err == nil {
			t.Fatal("want error for ram below minimum")
		}
	})

	t.Run("no recorded minimum accepts any override", func(t *testing.T) {
		// Linux bundles carry no minimums; the override stands.
		lin := &tart.Config{CPUCount: 4, MemorySize: 8 * gib}
		cpu, mem, err := resolveSizing(lin, BootOptions{CPUCount: 1, MemorySize: gib})
		if err != nil || cpu != 1 || mem != gib {
			t.Fatalf("cpu=%d mem=%d err=%v", cpu, mem, err)
		}
	})
}
