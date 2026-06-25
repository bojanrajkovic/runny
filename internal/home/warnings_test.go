package home

import (
	"strings"
	"testing"
)

// loadWarnings loads a config (defaults applied) and returns its warnings
// against the given host — the same order of operations the consumers use.
func loadWarnings(t *testing.T, body string, host HostResources) []Warning {
	t.Helper()
	c, err := LoadConfig(writeConfig(t, body))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return c.Warnings(host)
}

// generousHost outsizes anything the test configs ask for, so only the
// deliberately-overcommitted cases trip the resource check.
var generousHost = HostResources{LogicalCores: 16, PhysicalRAMGB: 64}

func TestWarningsSaneConfigNone(t *testing.T) {
	// minimalConfig: one darwin pool, count 2, no sizing override → counts at the
	// 2c/4GiB heuristic → 4 cores, 8 GiB, deadlines all default — nothing trips.
	if ws := loadWarnings(t, minimalConfig, generousHost); len(ws) != 0 {
		t.Fatalf("sane config: want 0 warnings, got %d: %+v", len(ws), ws)
	}
}

func TestWarningsCPUOvercommit(t *testing.T) {
	// 4 slots × 8 cores = 32 > 10 logical cores; RAM (4 × 4GiB heuristic = 16) fits.
	body := strings.Replace(minimalConfig, "count: 2", "count: 4\n    cpu_cores: 8", 1)
	ws := loadWarnings(t, body, HostResources{LogicalCores: 10, PhysicalRAMGB: 64})
	if len(ws) != 1 || ws[0].Kind != WarnResourceOvercommit {
		t.Fatalf("cpu overcommit: want one %s, got %+v", WarnResourceOvercommit, ws)
	}
	if m := strings.ToLower(ws[0].Message); !strings.Contains(m, "cpu") && !strings.Contains(m, "core") {
		t.Errorf("message should name the CPU axis: %q", ws[0].Message)
	}
}

func TestWarningsRAMOvercommit(t *testing.T) {
	// 4 slots × 32 GiB = 128 > 64; CPU (4 × 2c heuristic = 8) fits.
	body := strings.Replace(minimalConfig, "count: 2", "count: 4\n    ram_gb: 32", 1)
	ws := loadWarnings(t, body, HostResources{LogicalCores: 16, PhysicalRAMGB: 64})
	if len(ws) != 1 || ws[0].Kind != WarnResourceOvercommit {
		t.Fatalf("ram overcommit: want one %s, got %+v", WarnResourceOvercommit, ws)
	}
	if m := strings.ToLower(ws[0].Message); !strings.Contains(m, "ram") && !strings.Contains(m, "memory") {
		t.Errorf("message should name the RAM axis: %q", ws[0].Message)
	}
}

func TestWarningsOmittedSizingCountsAtHeuristic(t *testing.T) {
	// Two darwin pools, 4 slots total, no sizing overrides → 4 × 2c heuristic = 8 > 6.
	// Proves omitted cpu_cores/ram_gb are counted at the guest-sizing default.
	body := minimalConfig + `  - name: mac2
    os: darwin
    image: ghcr.io/cirruslabs/macos-tahoe-xcode:26.3
    count: 2
    target:
      owner: bojanrajkovic
      repo: mcp-paprika
    github:
      app_id: 222333
      private_key_path: /tmp/key2.pem
`
	ws := loadWarnings(t, body, HostResources{LogicalCores: 6, PhysicalRAMGB: 64})
	if len(ws) != 1 || ws[0].Kind != WarnResourceOvercommit {
		t.Fatalf("omitted-sizing overcommit: want one %s, got %+v", WarnResourceOvercommit, ws)
	}
}

func TestWarningsDeadlineTooShort(t *testing.T) {
	// await_ssh below the floor; everything else defaults (≥10s); host generous.
	body := minimalConfig + "deadlines:\n  await_ssh: 1s\n"
	ws := loadWarnings(t, body, generousHost)
	if len(ws) != 1 || ws[0].Kind != WarnDeadlineTooShort {
		t.Fatalf("short deadline: want one %s, got %+v", WarnDeadlineTooShort, ws)
	}
	if !strings.Contains(ws[0].Message, "await_ssh") {
		t.Errorf("message should name the offending deadline: %q", ws[0].Message)
	}
}

func TestWarningsDeadlineFloorBoundary(t *testing.T) {
	// Exactly at the floor is acceptable — only strictly-below warns.
	atFloor := minimalConfig + "deadlines:\n  await_ssh: 2s\n"
	if ws := loadWarnings(t, atFloor, generousHost); len(ws) != 0 {
		t.Fatalf("deadline at floor: want 0 warnings, got %+v", ws)
	}
}

func TestWarningsBothAxesOvercommit(t *testing.T) {
	// 4 slots × 8 cores = 32 > 10 AND 4 × 32 GiB = 128 > 64 → one warning per axis.
	body := strings.Replace(minimalConfig, "count: 2", "count: 4\n    cpu_cores: 8\n    ram_gb: 32", 1)
	ws := loadWarnings(t, body, HostResources{LogicalCores: 10, PhysicalRAMGB: 64})
	if len(ws) != 2 {
		t.Fatalf("both axes: want 2 warnings, got %d: %+v", len(ws), ws)
	}
	for _, w := range ws {
		if w.Kind != WarnResourceOvercommit {
			t.Errorf("want %s, got %s", WarnResourceOvercommit, w.Kind)
		}
	}
}

func TestWarningsUnknownHostSkipsOvercommit(t *testing.T) {
	// A zero host (probe failed/unavailable) must not manufacture overcommit
	// warnings — it can't compare against facts it doesn't have.
	body := strings.Replace(minimalConfig, "count: 2", "count: 4\n    cpu_cores: 8\n    ram_gb: 32", 1)
	if ws := loadWarnings(t, body, HostResources{}); len(ws) != 0 {
		t.Fatalf("unknown host: want 0 warnings, got %+v", ws)
	}
}
