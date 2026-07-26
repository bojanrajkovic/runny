// Package vm boots tart-format bundles as guest VMs, in-process — via
// Virtualization.framework on darwin (vz_darwin.go) or bare Hyper-V compute
// systems on windows (hcs_windows.go, ADR-0026). The Manager/Machine seam
// exists so the state machine tests against fakes on any OS; the real
// implementations are platform-specific.
package vm

import (
	"errors"
	"fmt"
	"runtime"
	"time"

	"github.com/bojanrajkovic/runny/internal/bounded"
	"github.com/bojanrajkovic/runny/internal/tart"
)

// ErrUnsupportedPlatform is returned when a boot is attempted on a build with
// no VM backend at all (the daemon's stub manager; see cmd/runnyd/platform_other.go).
var ErrUnsupportedPlatform = errors.New("vm boot requires darwin/arm64 or windows (see -doctor)")

// BootOptions configures one guest boot.
type BootOptions struct {
	// RunnerShareDir, when set, is shared read-only into the guest as a
	// virtiofs device tagged "runny-cache". It is the cycle's OWN per-slot
	// directory holding the single actions-runner tarball it cloned before
	// boot — never the shared download store, which is never mounted into a
	// guest (mounting one shared dir into every guest is the concurrent-delete
	// race the per-cycle clone exists to remove).
	RunnerShareDir string
	// CPUCount and MemorySize override the guest's sizing. Zero means "use
	// the bundle's baked config.json value". MemorySize is bytes. A request
	// below the bundle's recorded minimum is rejected, not clamped.
	CPUCount   uint
	MemorySize uint64
	// SSHUser/SSHPassword are the pool's configured guest credentials -- the
	// Hyper-V/HCS backend's Linux guests need them to log in over the console
	// and apply a one-time network fixup before returning (see
	// hcs_windows.go's fixupNetwork); unused on darwin (cloud-init's own
	// netplan already brings the guest's network up correctly) and unused
	// for the same backend's Windows guests (HNS's own pre-commit IP is
	// already correct, see hcs_windows.go's waitIPWindows).
	SSHUser     string
	SSHPassword string
}

// ShareTag is the virtiofs mount tag guests use:
// mount_virtiofs runny-cache /Volumes/runny-cache.
const ShareTag = "runny-cache"

// Machine is one running guest. Every method takes bounded.Context — nothing
// here may block indefinitely, and the type system enforces it.
type Machine interface {
	// MAC returns the guest's network MAC (fresh per boot).
	MAC() string
	// WaitIP polls the host's DHCP leases until the guest's MAC has one.
	WaitIP(ctx bounded.Context) (string, error)
	// Stop requests a graceful stop, waits up to grace, then force-stops.
	// It must not fail-and-leave-running: force is the floor.
	Stop(ctx bounded.Context, grace time.Duration) error
	// NeedsRunnerPush reports whether Boot could not attach RunnerShareDir as
	// a live guest share, meaning the runner tarball must instead be pushed
	// over the guest's own SSH channel before StartRunner runs. False on
	// darwin (virtiofs is live from Boot); true on windows (HCS's only
	// Linux-guest-capable share device, Plan9, needs LCOW's own guest-side
	// agent cooperation a bare compute system doesn't have — see hcs_windows.go).
	NeedsRunnerPush() bool
	// Spec reports what the guest ACTUALLY got, as resolved at Boot: the
	// bundle's baked values with the pool's overrides applied (resolveSizing),
	// plus the guest identity Boot validated against.
	//
	// It exists because neither input answers the question on its own. A pool
	// that sets no cpu_cores/ram_gb runs on whatever the image baked, so
	// reading the config tells you nothing; reading the image tells you nothing
	// once a pool overrides it. Only the resolved pair is the truth, and until
	// this existed it never left Boot.
	Spec() Spec
}

// Spec is a booted guest's resolved shape. Published on the cycle's telemetry
// and recorded in its cycle record, so "what did this guest get" is answerable
// after the fact rather than reconstructed from config and image by hand.
type Spec struct {
	// GuestOS and Arch are the bundle's, as validated at Boot.
	GuestOS string
	Arch    string
	// CPUCount and MemoryBytes are post-override (resolveSizing).
	CPUCount    uint
	MemoryBytes uint64
}

// Manager boots bundles.
type Manager interface {
	Boot(ctx bounded.Context, bundle tart.Bundle, opts BootOptions) (Machine, error)
	// ReapOrphans clears any per-slot backend state an unclean shutdown left
	// behind, for every entry under vmsDir, before the daemon's own startup
	// sweep (cmd/runnyd's sweepVMsDir) removes them — a backend whose orphan
	// still holds a file open can otherwise make that sweep fail to remove
	// that one slot, crash-looping the daemon forever on the exact same
	// lock. Best-effort: a single slot's failure to reap must not fail
	// ReapOrphans itself, or a wedged orphan becomes a wedged daemon
	// startup — exactly the failure mode this exists to kill. A no-op on
	// backends with no such orphan class (darwin's Virtualization.framework
	// releases everything on process exit; there is nothing to reap).
	ReapOrphans(vmsDir string) error
}

// checkHostArch rejects a bundle whose Arch doesn't match this process's own
// runtime.GOARCH — the host-capability half of guest validation that
// tart.Bundle.LoadConfig deliberately leaves to each platform's own Boot
// (LoadConfig is a portable shape check; neither Hyper-V nor
// Virtualization.framework cross-emulates architectures, so "can THIS host
// boot THIS arch" can only be answered here). Shared by vz_darwin.go and
// hcs_windows.go so the two platforms can't independently drift on this
// check the way they already did once (see ADR-0026).
func checkHostArch(cfg *tart.Config) error {
	if cfg.Arch != runtime.GOARCH {
		return fmt.Errorf("%w: this host is %s, bundle is %s/%s", tart.ErrUnsupportedGuest, runtime.GOARCH, cfg.OS, cfg.Arch)
	}
	return nil
}
