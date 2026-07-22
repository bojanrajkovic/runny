//go:build windows

package vm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/bojanrajkovic/runny/internal/bounded"
	"github.com/bojanrajkovic/runny/internal/winhcs/hcn"
	"github.com/bojanrajkovic/runny/internal/winhcs/hcs"
)

// reapOps are reapPriorSystem's real dependencies, split out so its decision
// logic — skip-if-not-found, fail-loudly-if-wait-times-out, proceed-if-found
// — is unit-testable off real hardware, the same split stop.go's stopOps
// already made for stopMachine (issue #320).
type reapOps interface {
	// openSystem returns the existing compute system for systemID, or
	// ok=false if none exists — reapPriorSystem's own tolerated case, not an
	// error.
	openSystem(ctx context.Context, systemID string) (sys reapSystem, ok bool, err error)
	// openEndpoint returns the existing HNS endpoint for endpointName, or
	// ok=false if none exists.
	openEndpoint(endpointName string) (ep reapEndpoint, ok bool, err error)
}

// reapSystem is the subset of *hcs.System reapPriorSystem needs. *hcs.System
// already has exactly this method set, so it satisfies reapSystem with no
// adapter.
type reapSystem interface {
	ID() string
	Terminate(ctx context.Context) error
	WaitCtx(ctx context.Context) error
	Close() error
}

// reapEndpoint is the subset of *hcn.HostComputeEndpoint reapPriorSystem
// needs — a method-based view since the real type exposes these as fields
// (Id, MacAddress), which hcnEndpoint adapts below.
type reapEndpoint interface {
	ID() string
	MAC() string
	Delete() error
}

// hcsReapOps is reapOps' real implementation, calling the vendored HCS/HNS
// bindings directly.
type hcsReapOps struct{}

func (hcsReapOps) openSystem(ctx context.Context, systemID string) (reapSystem, bool, error) {
	system, err := hcs.OpenComputeSystem(ctx, systemID)
	if err != nil {
		if hcs.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return system, true, nil
}

func (hcsReapOps) openEndpoint(endpointName string) (reapEndpoint, bool, error) {
	ep, err := hcn.GetEndpointByName(endpointName)
	if err != nil {
		var notFound hcn.EndpointNotFoundError
		if errors.As(err, &notFound) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return hcnEndpoint{ep}, true, nil
}

// hcnEndpoint adapts *hcn.HostComputeEndpoint's fields (Id, MacAddress) to
// reapEndpoint's method-based view.
type hcnEndpoint struct{ ep *hcn.HostComputeEndpoint }

func (e hcnEndpoint) ID() string    { return e.ep.Id }
func (e hcnEndpoint) MAC() string   { return e.ep.MacAddress }
func (e hcnEndpoint) Delete() error { return e.ep.Delete() }

// reapPriorSystem clears any compute system and/or HNS endpoint already
// registered under systemID/endpointName. Both are stable per slot (see the
// systemID comment in Boot), so an unclean prior shutdown -- a crash, kill
// -9, or a wedged Stop, none of which reach hcsMachine.destroy, and even a
// clean System.Close does not terminate a running system (see destroy's own
// doc comment) -- leaves a leftover that would otherwise make
// CreateComputeSystem/CreateEndpoint fail deterministically on every
// subsequent boot attempt for this slot, forever, with no cold-start
// recovery.
//
// Called two ways: from Boot, unconditionally at the top of every boot
// attempt for that one slot (bounding its own window separately from Boot's
// ctx, so a healthy boot's deadline budget is never eaten by the expected-
// rare case of nothing to reap); and from HCSManager.ReapOrphans at daemon
// startup, once per slot directory found under vms/, before the sweep that
// deletes them -- see ReapOrphans' own doc comment for why that ordering
// matters.
func reapPriorSystem(ops reapOps, systemID, endpointName string) error {
	ctx, cancel := bounded.WithTimeout(context.Background(), abandonedStopTimeout)
	defer cancel()

	system, ok, err := ops.openSystem(ctx, systemID)
	if err != nil {
		return fmt.Errorf("checking for a stale compute system %s: %w", systemID, err)
	}
	if ok {
		slog.Warn("reaping compute system left behind by an unclean shutdown", "id", systemID)
		if err := system.Terminate(ctx); err != nil {
			slog.Error("reaping stale compute system: terminate failed", "id", systemID, "err", err)
		}
		// Terminate only REQUESTS shutdown -- it can return before the system
		// actually exits -- so wait for the real exit notification before
		// closing the handle or touching its endpoint below; closing/deleting
		// out from under a guest that might still be running is the same
		// mistake hcsMachine.Stop's wedged-stop path already avoids. A wait
		// that doesn't resolve fails this reap loudly; the caller (Boot's own
		// backoff, or ReapOrphans' best-effort per-slot loop) decides what
		// happens next.
		if err := system.WaitCtx(ctx); err != nil {
			return fmt.Errorf("stale compute system %s did not exit within the reap window: %w", systemID, err)
		}
		if err := system.Close(); err != nil {
			slog.Error("reaping stale compute system: close failed", "id", systemID, "err", err)
		}
	}

	ep, ok, err := ops.openEndpoint(endpointName)
	if err != nil {
		return fmt.Errorf("checking for a stale HNS endpoint %q: %w", endpointName, err)
	}
	if !ok {
		return nil
	}
	slog.Warn("reaping HNS endpoint left behind by an unclean shutdown", "id", ep.ID(), "name", endpointName)
	mac := ep.MAC()
	if err := ep.Delete(); err != nil {
		slog.Error("reaping stale HNS endpoint: delete failed", "id", ep.ID(), "err", err)
		return nil
	}
	scrubNeighborEntry(mac)
	return nil
}

// reapOrphansTimeout bounds reapAllSlots' entire pass over every slot found
// under vms/ -- a startup-time operation with no per-cycle deadline to
// inherit, so it needs its own ceiling the way abandonedStopTimeout already
// does for a single reap. Sized as a healthy multiple of a single slot's own
// reap window (abandonedStopTimeout), not summed across an arbitrary slot
// count: a fleet with more stale slots than this budget allows degrades to
// "some slots reaped, rest logged for the next restart," never to an
// unbounded startup hang. A var, not a const, so tests can shrink it.
var reapOrphansTimeout = 5 * time.Minute

// ReapOrphans reaps every slot directory's stale compute system/HNS
// endpoint left behind by an unclean shutdown, before the daemon's own
// startup sweep (cmd/runnyd/main.go's os.RemoveAll(dir.VMsDir())) deletes
// them -- otherwise that sweep can hit a file still held open by an
// orphaned compute system and fail outright (not skip-and-continue),
// crash-looping the daemon forever with no cold-start recovery (issue
// #320): each restart hits the identical lock, since nothing reaped the
// orphan in between.
func (HCSManager) ReapOrphans(vmsDir string) error {
	return reapAllSlots(hcsReapOps{}, vmsDir)
}

// reapAllSlots is ReapOrphans' actual logic, split out so it's unit-testable
// with a fake reapOps and a real (but disposable, t.TempDir()) vmsDir --
// only reapPriorSystem's own real calls need hardware, not the enumeration
// or best-effort-continue decisions made here.
//
// Best-effort per slot, by design: a slot whose stale system won't exit
// within its own reap window is logged and skipped, never fatal to this
// call — a wedged orphan must not become a wedged daemon startup, which is
// exactly the failure mode this exists to kill. Slot names double as HCS
// systemIDs (Boot derives systemID the same way, from the slot's own vmDir
// basename — see Boot's systemID comment), so listing vmsDir's entries is
// sufficient; no cross-reference against the config's pools is needed.
func reapAllSlots(ops reapOps, vmsDir string) error {
	entries, err := os.ReadDir(vmsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("listing %s for orphan reap: %w", vmsDir, err)
	}

	ctx, cancel := bounded.WithTimeout(context.Background(), reapOrphansTimeout)
	defer cancel()
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		select {
		case <-ctx.Done():
			slog.Error("startup: orphan reap pass ran out of time; remaining slots left for a later restart", "err", ctx.Err())
			return nil
		default:
		}
		systemID := e.Name()
		if err := reapPriorSystem(ops, systemID, "runny-"+systemID); err != nil {
			slog.Error("startup: reaping orphaned compute system failed; a later restart or manual cleanup may be needed", "slot", systemID, "err", err)
		}
	}
	return nil
}
