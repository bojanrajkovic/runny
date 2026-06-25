package home

import (
	"fmt"
	"time"
)

// Warning is a non-fatal config soft-validation result: the config parses and is
// structurally valid, but a value is almost certainly an operator mistake.
// Warnings never block a load — they feed the OK/Warn/Error verdict of the
// config-compat gate. Kind is a stable, machine-readable class (part of the
// -test-config JSON contract the Swift app and runnyctl both parse); Message is
// the human-readable detail.
type Warning struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// Warning kinds. These are contract surface (the -test-config JSON): a consumer
// may branch on them, so they stay stable across config-schema revisions.
const (
	// WarnResourceOvercommit: summed across all concurrent slots, the config
	// claims more CPU or RAM than the host has. The axis is named in Message.
	WarnResourceOvercommit = "resource-overcommit"
	// WarnDeadlineTooShort: a deadlines.* value below a conservative floor —
	// almost always a typo that would spuriously time out every cycle.
	WarnDeadlineTooShort = "deadline-too-short"
)

// HostResources are the local facts the resource-overcommit soft-validation
// compares against. They are injected, never probed inside this
// platform-agnostic loader: the daemon fills them from a darwin probe, tests
// from synthetic values. A zero field means "unknown" and disables that axis.
type HostResources struct {
	LogicalCores  int
	PhysicalRAMGB uint
}

// Guest-sizing baseline for a pool that omits cpu_cores/ram_gb. The true
// per-guest size is baked in the image's config.json (e.g. cirruslabs 2c/4GiB)
// and is unknowable without a network pull, which the gate forbids — so the
// aggregate-overcommit warning assumes this conservative baseline.
// ponytail: heuristic baseline for a soft-validation, not a precise gate; bump
// if the common image baseline changes.
const (
	defaultGuestCores = 2
	defaultGuestRAMGB = 4
)

// deadlineFloor is the conservative lower bound for any deadlines.* value. The
// smallest real default deadline is clone at 10s, so this false-positives
// nothing legitimate while catching the absurd typo (e.g. await_ssh: 1s).
// ponytail: one flat floor for all deadlines; per-field floors would be
// speculative precision.
const deadlineFloor = 2 * time.Second

// Warnings runs the non-fatal soft-validations over an already-validated config
// against the host's resources, returning the operator footguns that parse but
// are almost certainly mistakes. Pure and deterministic: same (config, host)
// yields the same warnings, in a stable order. No network or platform access —
// host facts are injected.
func (c *Config) Warnings(host HostResources) []Warning {
	var ws []Warning

	// Aggregate resource overcommit: summed across every slot, do the pools ask
	// for more than the host has? Per-guest fit is validated at boot; this is
	// the aggregate the boot check can't see. Pools omitting a sizing override
	// count at the guest-sizing baseline; a zero host fact disables that axis.
	var sumCores, sumRAMGB uint
	for _, p := range c.Pools {
		cores, ram := p.CPUCores, p.RAMGB
		if cores == 0 {
			cores = defaultGuestCores
		}
		if ram == 0 {
			ram = defaultGuestRAMGB
		}
		sumCores += uint(p.Count) * cores
		sumRAMGB += uint(p.Count) * ram
	}
	if host.LogicalCores > 0 && sumCores > uint(host.LogicalCores) {
		ws = append(ws, Warning{
			Kind: WarnResourceOvercommit,
			Message: fmt.Sprintf(
				"pools request %d CPU cores across all slots but the host has %d logical cores",
				sumCores, host.LogicalCores,
			),
		})
	}
	if host.PhysicalRAMGB > 0 && sumRAMGB > host.PhysicalRAMGB {
		ws = append(ws, Warning{
			Kind: WarnResourceOvercommit,
			Message: fmt.Sprintf(
				"pools request %d GiB RAM across all slots but the host has %d GiB",
				sumRAMGB, host.PhysicalRAMGB,
			),
		})
	}

	// Deadlines below the floor, checked in a fixed order for deterministic
	// output. Limits are deliberately excluded — the floor reflects guest-op
	// latency, not the much larger limit budgets (max_job_duration, max_idle).
	for _, f := range []struct {
		name string
		d    Duration
	}{
		{"deadlines.clone", c.Deadlines.Clone},
		{"deadlines.boot", c.Deadlines.Boot},
		{"deadlines.await_ip", c.Deadlines.AwaitIP},
		{"deadlines.await_ssh", c.Deadlines.AwaitSSH},
		{"deadlines.mint_jit", c.Deadlines.MintJIT},
		{"deadlines.provision", c.Deadlines.Provision},
		{"deadlines.teardown", c.Deadlines.Teardown},
		{"deadlines.secure_ssh", c.Deadlines.SecureSSH},
		{"deadlines.pull_stall", c.Deadlines.PullStall},
		{"deadlines.resolve", c.Deadlines.Resolve},
	} {
		if f.d > 0 && f.d.D() < deadlineFloor {
			ws = append(ws, Warning{
				Kind: WarnDeadlineTooShort,
				Message: fmt.Sprintf(
					"%s is %v, below the %v floor — likely a typo that will time out every cycle",
					f.name, f.d.D(), deadlineFloor,
				),
			})
		}
	}
	return ws
}
