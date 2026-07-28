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
// already made for stopMachine.
type reapOps interface {
	// openSystem returns the existing compute system for systemID, or a nil
	// reapSystem if none exists — reapPriorSystem's own tolerated case, not
	// an error.
	openSystem(ctx bounded.Context, systemID string) (reapSystem, error)
	// openEndpoint returns the existing HNS endpoint for endpointName, or a
	// nil reapEndpoint if none exists. Takes a ctx because the real
	// implementation is a context-free HNS RPC that a wedged HNS service never
	// returns from — and this runs at daemon startup, where a hang has no
	// outer deadline to save it.
	openEndpoint(ctx bounded.Context, endpointName string) (reapEndpoint, error)
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

func (hcsReapOps) openSystem(ctx bounded.Context, systemID string) (reapSystem, error) {
	system, err := hcs.OpenComputeSystem(ctx, systemID)
	if err != nil {
		if hcs.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return system, nil
}

func (hcsReapOps) openEndpoint(ctx bounded.Context, endpointName string) (reapEndpoint, error) {
	ep, err := awaitBounded(ctx, func() (*hcn.HostComputeEndpoint, error) {
		return hcn.GetEndpointByName(endpointName)
	}, nil) // a lookup allocates nothing
	if err != nil {
		var notFound hcn.EndpointNotFoundError
		if errors.As(err, &notFound) {
			return nil, nil
		}
		return nil, err
	}
	return hcnEndpoint{ep}, nil
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

	system, err := ops.openSystem(ctx, systemID)
	if err != nil {
		return fmt.Errorf("checking for a stale compute system %s: %w", systemID, err)
	}
	if system != nil {
		slog.Warn("reaping compute system left behind by an unclean shutdown", "id", systemID)
		// terminateAndClose fails loudly only if WaitCtx never confirms
		// exit; the caller (Boot's own backoff, or ReapOrphans' best-effort
		// per-slot loop) decides what happens next.
		if err := terminateAndClose(ctx, system, "reaping stale compute system"); err != nil {
			return err
		}
	}

	ep, err := ops.openEndpoint(ctx, endpointName)
	if err != nil {
		return fmt.Errorf("checking for a stale HNS endpoint %q: %w", endpointName, err)
	}
	if ep == nil {
		return nil
	}
	slog.Warn("reaping HNS endpoint left behind by an unclean shutdown", "id", ep.ID(), "name", endpointName)
	deleteEndpointAndScrub(ctx, ep, "reaping stale HNS endpoint")
	return nil
}

// terminateAndClose runs terminate -> wait -> close, shared by
// reapPriorSystem and hcs_windows.go's abandonComputeSystem -- both need the
// same ordering for the same reason: Terminate only REQUESTS shutdown, so
// this waits for the real exit notification (bounded by ctx) before closing
// the handle -- closing out from under a guest that might still be running
// is the mistake hcsMachine.Stop's wedged-stop path already avoids. Only a
// WaitCtx failure is returned to the caller; Terminate/Close failures are
// logged, not fatal, since WaitCtx is the sole authority on whether it was
// safe to proceed. label distinguishes the two callers' log lines (the
// underlying system is the same either way, but "reap" and "abandon" name
// different circumstances).
func terminateAndClose(ctx context.Context, sys reapSystem, label string) error {
	if err := sys.Terminate(ctx); err != nil {
		slog.Error(label+": terminate failed", "id", sys.ID(), "err", err)
	}
	if err := sys.WaitCtx(ctx); err != nil {
		return fmt.Errorf("%s %s did not exit within the reap window: %w", label, sys.ID(), err)
	}
	if err := sys.Close(); err != nil {
		slog.Error(label+": close failed", "id", sys.ID(), "err", err)
	}
	return nil
}

// deleteEndpointAndScrub deletes ep and scrubs its neighbor-table entry --
// shared by reapPriorSystem and abandonComputeSystem, the two paths that
// both attach an endpoint before either can be reached (see
// scrubNeighborEntry's own doc comment). Best-effort: a delete failure is
// logged, not returned -- there's nothing further either caller could do
// differently.
func deleteEndpointAndScrub(ctx bounded.Context, ep reapEndpoint, label string) {
	if err := deleteAndScrub(ctx, ep.Delete, ep.MAC()); err != nil {
		slog.Error(label+": delete did not confirm before the window closed", "id", ep.ID(), "err", err)
	}
}

// deleteAndScrub bounds an endpoint delete and scrubs that MAC's stale neighbor
// rows exactly when the delete LANDS -- including when it lands after we gave up
// waiting, which is why the scrub is the abandon callback and not a statement
// after the call.
//
// Bounding the wait does not cancel the delete: the goroutine runs it to
// completion regardless. So a slow-but-healthy HNS would otherwise delete the
// endpoint a moment after we returned, and nothing would ever scrub its rows --
// reapPriorSystem early-returns when the endpoint is already gone, so no later
// reap reaches the scrub either. HNS never clears those rows itself (see
// internal/vm/CLAUDE.md), so they would accumulate one per boot cycle.
func deleteAndScrub(ctx bounded.Context, del func() error, mac string) error {
	_, err := awaitBounded(ctx, func() (struct{}, error) { return struct{}{}, del() },
		func(struct{}) { scrubNeighborEntry(mac) })
	if err == nil {
		scrubNeighborEntry(mac)
	}
	return err
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
// startup sweep (cmd/runnyd/main.go's sweepVMsDir) removes them -- otherwise
// that sweep can hit a file still held open by an orphaned compute system
// and fail to remove that one slot, crash-looping the daemon forever on the
// exact same lock with no cold-start recovery: nothing else ever reaps the
// orphan in between restarts.
func (HCSManager) ReapOrphans(vmsDir, instancePrefix string) error {
	return reapAllSlots(hcsReapOps{}, vmsDir, instancePrefix)
}

// slotSystemID is the single derivation of a slot's HCS compute-system ID, used
// by Boot and by the orphan reap so the two can never disagree about what a
// slot is called.
//
// HCS system IDs and HNS endpoint names are host-global, while a slot name is
// only unique within one daemon's home. Mixing in the install's own prefix —
// the same one runner names carry — keeps two daemons on one host from deriving
// the same identifier for different guests, which would otherwise let each
// daemon's unconditional pre-boot reap terminate the other's live guest and log
// it as routine cleanup.
//
// An empty prefix yields the bare slot name, which is what the unit tests and
// any caller without a resolved home get.
func slotSystemID(instancePrefix, slot string) string {
	if instancePrefix == "" {
		return slot
	}
	return instancePrefix + "-" + slot
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
func reapAllSlots(ops reapOps, vmsDir, instancePrefix string) error {
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
		if err := ctx.Err(); err != nil {
			slog.Error("startup: orphan reap pass ran out of time; remaining slots left for a later restart", "err", err)
			return nil
		}
		systemID := slotSystemID(instancePrefix, e.Name())
		if err := reapPriorSystem(ops, systemID, "runny-"+systemID); err != nil {
			slog.Error("startup: reaping orphaned compute system failed; a later restart or manual cleanup may be needed", "slot", systemID, "err", err)
		}
	}
	return nil
}
