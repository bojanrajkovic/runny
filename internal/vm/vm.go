// Package vm boots tart-format bundles as macOS guests, in-process via
// Virtualization.framework (ADR-0008). The Manager/Machine seam exists so the
// state machine tests against fakes on any OS; the real implementation is
// darwin-only.
package vm

import (
	"errors"
	"time"

	"github.com/bojanrajkovic/runny/internal/bounded"
	"github.com/bojanrajkovic/runny/internal/tart"
)

// ErrUnsupportedPlatform is returned when a boot is attempted on a
// non-darwin build (the daemon's stub manager; see cmd/runnyd).
var ErrUnsupportedPlatform = errors.New("vm boot requires darwin/arm64 (see -doctor)")

// BootOptions configures one guest boot.
type BootOptions struct {
	// RunnerCacheDir, when set, is shared read-only into the guest as a
	// virtiofs device tagged "runny-cache" — the actions-runner tarball
	// cache, downloaded once per version on the host.
	RunnerCacheDir string
}

// ShareTag is the virtiofs mount tag guests use:
// mount_virtiofs runny-cache /Volumes/runny-cache.
const ShareTag = "runny-cache"

// Machine is one running guest. Every method takes bounded.Context — nothing
// here may block indefinitely, and the type system enforces it (ADR-0011).
type Machine interface {
	// MAC returns the guest's network MAC (fresh per boot).
	MAC() string
	// WaitIP polls the host's DHCP leases until the guest's MAC has one.
	WaitIP(ctx bounded.Context) (string, error)
	// Stop requests a graceful stop, waits up to grace, then force-stops.
	// It must not fail-and-leave-running: force is the floor (ADR-0004).
	Stop(ctx bounded.Context, grace time.Duration) error
	// Done is closed when the guest stops for any reason.
	Done() <-chan struct{}
}

// Manager boots bundles.
type Manager interface {
	Boot(ctx bounded.Context, bundle tart.Bundle, opts BootOptions) (Machine, error)
}
