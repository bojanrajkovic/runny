//go:build windows

package vm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"path/filepath"
	"time"

	"github.com/bojanrajkovic/runny/internal/bounded"
	"github.com/bojanrajkovic/runny/internal/tart"
	"github.com/bojanrajkovic/runny/internal/winhcs/hcn"
	"github.com/bojanrajkovic/runny/internal/winhcs/hcs"
	hcsschema "github.com/bojanrajkovic/runny/internal/winhcs/hcs/schema2"
	"github.com/bojanrajkovic/runny/internal/winhcs/osversion"
)

const (
	// secureBootTemplateID is the Microsoft UEFI Certificate Authority
	// template GUID -- the Linux-shim trust anchor. Validated at schema 2.1
	// on real hardware (issue #308); do not change without re-validating.
	secureBootTemplateID = "272e7447-90a4-4563-a4b9-8e4ab00526ce"
	defaultSwitchName    = "Default Switch"

	// abandonedStopTimeout bounds the best-effort cleanup of a compute
	// system whose Start blew its deadline — the same role
	// vz_darwin.go's own abandonedStopTimeout plays for a wedged boot there,
	// duplicated rather than shared since it's darwin-build-tagged. Also
	// bounds reapPriorSystem's own cleanup of a system/endpoint left behind
	// by an unclean prior shutdown — the same class of orphan, reaped at a
	// different point in the lifecycle.
	abandonedStopTimeout = 30 * time.Second

	// forceStopTimeout bounds hcsStopOps.forceStop's own Terminate call — see
	// its doc comment for why this can't reuse the Stop call's outer ctx.
	forceStopTimeout = 30 * time.Second
)

// HCSManager is the real Manager on windows: Hyper-V compute systems via the
// vendored winhcs binding. Mirrors vz_darwin.go's shape and boot recipe —
// this is its Hyper-V sibling, issue #308.
type HCSManager struct{}

var _ Manager = HCSManager{}

// Boot builds the compute-system document from the bundle's tart config and
// starts the guest. Unlike vz_darwin.go, it never creates the differencing
// disk itself -- CLONE already left one at bundle.VHDXPath() (internal/tart.
// CloneVHDX) by the time Boot runs; Boot only ever attaches it.
func (m HCSManager) Boot(ctx bounded.Context, bundle tart.Bundle, opts BootOptions) (Machine, error) {
	cfg, err := bundle.LoadConfig()
	if err != nil {
		return nil, err
	}
	// Hyper-V's HCS path here is Linux-only (issue #308 never describes
	// booting a darwin guest on Hyper-V, which has no macOS boot support at
	// all); checkHostArch (shared with vz_darwin.go) covers the arch half.
	if cfg.OS != "linux" {
		return nil, fmt.Errorf("%w: this backend only boots linux guests, bundle is %s/%s", tart.ErrUnsupportedGuest, cfg.OS, cfg.Arch)
	}
	if err := checkHostArch(cfg); err != nil {
		return nil, err
	}
	// Schema 2.1 is the validated recipe (issue #308); RS5/LTSC2019 is
	// roughly its floor. A build below that gets a clear, upfront error
	// instead of a confusing HCS document-validation failure.
	if osversion.Build() < osversion.RS5 {
		return nil, fmt.Errorf("Hyper-V VM backend needs schema 2.1 (Windows build %d or later); this host is build %d", osversion.RS5, osversion.Build())
	}

	network, err := hcn.GetNetworkByName(defaultSwitchName)
	if err != nil {
		return nil, fmt.Errorf("resolving %q: %w", defaultSwitchName, err)
	}

	// systemID is the slot dir name -- stable across every cycle that slot
	// runs, NOT unique per cycle (the slot's own vmDir is reused cycle over
	// cycle; only the cycle's own artifact dir under it gets a fresh random
	// id). An unclean prior shutdown can leave a compute system and/or HNS
	// endpoint registered under this exact ID/name, which would otherwise
	// make CreateComputeSystem/CreateEndpoint below fail deterministically
	// on every subsequent boot attempt for this slot -- reapPriorSystem
	// clears that the same way CLONE's own RemoveAll(vmDir) self-heals a
	// surviving clone file.
	systemID := filepath.Base(string(bundle))
	endpointName := "runny-" + systemID
	if err := reapPriorSystem(systemID, endpointName); err != nil {
		return nil, err
	}

	ep, err := network.CreateEndpoint(&hcn.HostComputeEndpoint{
		Name:               endpointName,
		HostComputeNetwork: network.Id,
		SchemaVersion:      hcn.V2SchemaVersion(),
	})
	if err != nil {
		return nil, fmt.Errorf("creating HNS endpoint: %w", err)
	}

	cpu, mem, err := resolveSizing(cfg, opts)
	if err != nil {
		deleteEndpoint(ep)
		return nil, err
	}
	// HCS's schema takes memory as whole MiB and CPU count as uint32; neither
	// resolveSizing (a floor-only check, shared with darwin which needs no
	// such conversion) nor LoadConfig validates either shape, so a
	// non-whole-MiB memorySize or a CPU count above uint32's range would
	// otherwise truncate/wrap silently into the document sent to HCS.
	if mem%(1<<20) != 0 {
		deleteEndpoint(ep)
		return nil, fmt.Errorf("memory size %d bytes is not a whole number of MiB", mem)
	}
	if cpu > math.MaxUint32 {
		deleteEndpoint(ep)
		return nil, fmt.Errorf("cpu_cores %d exceeds the maximum HCS can express", cpu)
	}

	doc := &hcsschema.ComputeSystem{
		Owner:         "runny",
		SchemaVersion: &hcsschema.Version{Major: 2, Minor: 1},
		VirtualMachine: &hcsschema.VirtualMachine{
			Chipset: &hcsschema.Chipset{
				Uefi: &hcsschema.Uefi{
					ApplySecureBootTemplate: "Apply",
					SecureBootTemplateId:    secureBootTemplateID,
					// BootThis deliberately OMITTED: UEFI firmware
					// auto-discovers the ESP on the SCSI-attached disk. The
					// documented ScsiDrive/File DeviceType values are
					// unexercised even in Microsoft's own code and do not
					// work (validated the hard way, issue #308). Do not
					// "fix" this.
				},
			},
			ComputeTopology: &hcsschema.Topology{
				Memory:    &hcsschema.VirtualMachineMemory{SizeInMB: mem >> 20},
				Processor: &hcsschema.VirtualMachineProcessor{Count: uint32(cpu)},
			},
			Devices: &hcsschema.Devices{
				// Populated, not nil: an empty Devices fails with the
				// generic "The data is invalid" (validated, issue #308).
				HvSocket: &hcsschema.HvSocket2{HvSocketConfig: &hcsschema.HvSocketSystemConfig{}},
				Scsi: map[string]hcsschema.Scsi{
					"0": {Attachments: map[string]hcsschema.Attachment{
						"0": {Type_: "VirtualDisk", Path: bundle.VHDXPath()},
					}},
				},
				ComPorts: map[string]hcsschema.ComPort{
					"0": {NamedPipe: consolePipeName(systemID)},
				},
				NetworkAdapters: map[string]hcsschema.NetworkAdapter{
					"0": {EndpointId: ep.Id, MacAddress: ep.MacAddress},
				},
			},
		},
	}

	// hcs.CreateComputeSystem/System.Start take ctx but the vendored package's
	// own async-completion wait (waitForNotification) never actually selects
	// on ctx.Done() -- only the real HCS notification or its own internal
	// multi-minute timer -- so without this wrapping, a merely-slow (not
	// wedged) host would block this whole call, and the FSM goroutine with
	// it, well past the configured BOOT deadline. Bound OUR wait the same
	// way vz_darwin.go bounds its own context-free VZ Start call: if the
	// vendored call succeeds after we've already given up, nothing owns the
	// result (Boot already returned an error), so a detached goroutine tears
	// it down once it resolves, mirroring vz_darwin.go's abandoned-boot path.
	type createResult struct {
		system *hcs.System
		err    error
	}
	createCh := make(chan createResult, 1)
	go func() {
		system, err := hcs.CreateComputeSystem(ctx, systemID, doc)
		createCh <- createResult{system, err}
	}()

	var system *hcs.System
	select {
	case res := <-createCh:
		if res.err != nil {
			deleteEndpoint(ep)
			return nil, fmt.Errorf("CreateComputeSystem: %w", res.err)
		}
		system = res.system
	case <-ctx.Done():
		go func() {
			if res := <-createCh; res.err == nil {
				abandonComputeSystem(res.system, ep)
			} else {
				deleteEndpoint(ep)
			}
		}()
		return nil, fmt.Errorf("CreateComputeSystem: %w", ctx.Err())
	}

	startErrCh := runAsync(func() error { return system.Start(ctx) })
	select {
	case err := <-startErrCh:
		if err != nil {
			abandonComputeSystem(system, ep)
			return nil, fmt.Errorf("Start: %w", err)
		}
	case <-ctx.Done():
		go func() {
			<-startErrCh // whichever way Start eventually resolves, it must still be torn down
			abandonComputeSystem(system, ep)
		}()
		return nil, fmt.Errorf("Start: %w", ctx.Err())
	}

	return &hcsMachine{system: system, endpoint: ep, mac: ep.MacAddress}, nil
}

// consolePipeName is per-system so concurrent slots never collide on the
// same named pipe. Nothing in this backend dials it today (the guest self-
// configures networking with no console interaction needed — see
// hcsMachine's WaitIP doc comment) — it exists for operator/debug access to
// the boot console, the same reason vz guests get a display even headless.
func consolePipeName(systemID string) string {
	return `\\.\pipe\runny-console-` + systemID
}

// deleteEndpoint is the plain best-effort delete: used wherever nothing
// downstream depends on whether it succeeded. abandonComputeSystem and
// hcsMachine.destroy need the error itself (to decide whether to scrub the
// neighbor-table entry below), so they call ep.Delete() directly instead.
func deleteEndpoint(ep *hcn.HostComputeEndpoint) {
	if err := ep.Delete(); err != nil {
		slog.Error("abandoned HNS endpoint: delete failed; endpoint may leak until manually cleaned up", "id", ep.Id, "err", err)
	}
}

// scrubNeighborEntry removes the stale Permanent neighbor-table entry HNS
// leaves behind for mac once its endpoint is gone — see hcsMachine.destroy's
// doc comment for why this doesn't happen on its own. Shared by every
// cleanup path that deletes an endpoint (a boot that fails at Start, and a
// confirmed-stopped Machine both attach the endpoint before either one is
// reached, so both can leak the same entry). Best-effort: only logs, since
// by this point the endpoint is already gone and there's nothing further a
// caller could do differently.
func scrubNeighborEntry(mac string) {
	entries, err := readNeighborEntries()
	if err != nil {
		slog.Error("teardown: reading neighbor table for stale-entry scrub failed", "mac", mac, "err", err)
		return
	}
	for _, e := range entries {
		if e.permanent() && normalizeMAC(e.mac) == normalizeMAC(mac) {
			if err := deleteNeighborEntry(e.ip, e.interfaceIndex); err != nil {
				slog.Error("teardown: stale neighbor-table entry delete failed", "ip", e.ip, "mac", mac, "err", err)
			}
			return
		}
	}
}

// reapPriorSystem clears any compute system and/or HNS endpoint already
// registered under systemID/endpointName before Boot creates fresh ones.
// Both are stable per slot (see the systemID comment in Boot), so an
// unclean prior shutdown -- a crash, kill -9, or a wedged Stop, none of
// which reach hcsMachine.destroy, and even a clean System.Close does not
// terminate a running system (see destroy's own doc comment) -- leaves a
// leftover that would otherwise make CreateComputeSystem/CreateEndpoint
// fail deterministically on every subsequent boot attempt for this slot,
// forever, with no cold-start recovery. Uses its own bounded window rather
// than Boot's ctx, matching abandonComputeSystem and forceStop's own
// reasoning: this runs unconditionally at the top of every Boot, so it
// must never eat into a healthy boot's deadline budget on the (expected to
// be rare) path where there's nothing to reap.
func reapPriorSystem(systemID, endpointName string) error {
	ctx, cancel := bounded.WithTimeout(context.Background(), abandonedStopTimeout)
	defer cancel()

	system, sysErr := hcs.OpenComputeSystem(ctx, systemID)
	if sysErr != nil && !hcs.IsNotExist(sysErr) {
		return fmt.Errorf("checking for a stale compute system %s: %w", systemID, sysErr)
	}
	if system != nil {
		slog.Warn("reaping compute system left behind by an unclean shutdown", "id", systemID)
		if err := system.Terminate(ctx); err != nil {
			slog.Error("reaping stale compute system: terminate failed", "id", systemID, "err", err)
		}
		if err := system.Close(); err != nil {
			slog.Error("reaping stale compute system: close failed", "id", systemID, "err", err)
		}
	}

	ep, epErr := hcn.GetEndpointByName(endpointName)
	if epErr != nil {
		var notFound hcn.EndpointNotFoundError
		if errors.As(epErr, &notFound) {
			return nil
		}
		return fmt.Errorf("checking for a stale HNS endpoint %q: %w", endpointName, epErr)
	}
	slog.Warn("reaping HNS endpoint left behind by an unclean shutdown", "id", ep.Id, "name", endpointName)
	mac := ep.MacAddress
	if err := ep.Delete(); err != nil {
		slog.Error("reaping stale HNS endpoint: delete failed", "id", ep.Id, "err", err)
		return nil
	}
	scrubNeighborEntry(mac)
	return nil
}

// abandonComputeSystem is the failed-Start cleanup: nothing owns system or
// ep once Boot returns an error (the caller never got a Machine), so Boot
// must tear them down itself here or they leak — the same class of orphan
// this backend's own hardware spike found accumulating from earlier manual
// runs (issue #308). The endpoint is already wired into the compute-system
// document, and CreateComputeSystem already succeeded, before Start is ever
// called — so a Start that fails can still leak the same stale
// neighbor-table entry a confirmed-stopped Machine's destroy() scrubs;
// scrubNeighborEntry is shared for exactly that reason.
func abandonComputeSystem(system *hcs.System, ep *hcn.HostComputeEndpoint) {
	ctx, cancel := bounded.WithTimeout(context.Background(), abandonedStopTimeout)
	defer cancel()
	if err := system.Terminate(ctx); err != nil {
		slog.Error("abandoned compute system: terminate failed", "id", system.ID(), "err", err)
	}
	if err := system.Close(); err != nil {
		slog.Error("abandoned compute system: close failed", "id", system.ID(), "err", err)
	}
	mac := ep.MacAddress
	if err := ep.Delete(); err != nil {
		slog.Error("abandoned HNS endpoint: delete failed; endpoint may leak until manually cleaned up", "id", ep.Id, "err", err)
		return
	}
	scrubNeighborEntry(mac)
}

type hcsMachine struct {
	system   *hcs.System
	endpoint *hcn.HostComputeEndpoint
	mac      string
}

func (m *hcsMachine) MAC() string { return m.mac }

// WaitIP polls the host IP neighbor table for a Permanent entry matching
// this machine's MAC. HNS pre-commits the MAC->IP binding at endpoint
// attach (validated on real hardware, issues #307/#308): the entry appears
// within seconds of Start, well before the guest OS could have booted, and
// carries exactly the IP the guest's own DHCP will obtain. This backend
// does no console interaction to get there — the validated guest image's
// cloud-init generates its own network config dynamically for whatever NIC
// is actually present (confirmed: /etc/netplan/50-cloud-init.yaml already
// matches eth0 with dhcp4: true, no live fixup needed). If a future image
// ships a static installer-time netplan that doesn't self-adapt, WaitIP
// simply times out — that's the signal to revisit this, not something to
// defend against speculatively now.
func (m *hcsMachine) WaitIP(ctx bounded.Context) (string, error) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("waiting for neighbor-table entry for %s: %w", m.mac, ctx.Err())
		case <-m.system.WaitChannel():
			return "", fmt.Errorf("guest stopped while waiting for IP")
		case <-t.C:
			entries, err := readNeighborEntries()
			if err != nil {
				continue // transient GetIpNetTable2 failure; next tick retries
			}
			if ip, ok := findPermanentIP(entries, m.mac); ok {
				return ip, nil
			}
		}
	}
}

// Stop runs the bounded stop sequence every platform shares (stop.go):
// Shutdown is graceful but the guest often ignores it (issue #308), so
// force is the floor, same as darwin's RequestStop. Unlike vzMachine, this
// is also the only teardown hook the Machine interface gives HCS resources
// — a *vz.VirtualMachine releases on GC, but an HCS compute system's handle
// and its HNS endpoint are real OS/service-level resources that don't. So
// once stopMachine confirms the guest actually reached a terminal state,
// Stop also closes the compute system, deletes the endpoint, and scrubs the
// stale neighbor-table entry HNS leaves behind (destroy-and-recycle, never
// in-place repair — the slot's differencing-child VHDX itself is removed by
// the existing vms/ sweep, not here). A wedged stop (stopMachine's error
// case, guest possibly still running) skips all of that: closing the handle
// or deleting the endpoint out from under a guest that might still be
// running is the same class of mistake the FSM's own teardown avoids by
// never deleting a wedged cycle's clone dir.
func (m *hcsMachine) Stop(ctx bounded.Context, grace time.Duration) error {
	if err := stopMachine(ctx, grace, m.system.WaitChannel(), hcsStopOps{system: m.system, ctx: ctx}); err != nil {
		return err
	}
	m.destroy()
	return nil
}

// destroy is the confirmed-stopped cleanup path: best-effort, since the
// guest is already gone and a failure here leaves only an orphan for a
// later manual sweep, never a wedge (mirrors run.go's own "steps 4/5 are
// best-effort cleanups" teardown reasoning). The neighbor-table entry HNS
// programmed for this endpoint does NOT clear on its own when the endpoint
// is deleted (validated empirically, issue #308's hardware spike — it
// outlived Terminate, endpoint Delete, and 30s of polling after both), so
// scrubNeighborEntry (shared with abandonComputeSystem) scrubs it
// explicitly once the endpoint is confirmed gone.
func (m *hcsMachine) destroy() {
	if err := m.system.Close(); err != nil {
		slog.Error("teardown: closing compute system failed", "id", m.system.ID(), "err", err)
	}
	if err := m.endpoint.Delete(); err != nil {
		slog.Error("teardown: deleting HNS endpoint failed", "id", m.endpoint.Id, "err", err)
		return
	}
	scrubNeighborEntry(m.mac)
}

// hcsStopOps closes over the Stop call's own ctx: hcs.System.Shutdown/
// Terminate need one, unlike vz's context-free RequestStop/Stop, but
// stopOps' methods take none — a small adapter is simpler than threading
// ctx through hcsMachine as mutable state.
type hcsStopOps struct {
	system *hcs.System
	ctx    context.Context
}

func (o hcsStopOps) requestStop() (bool, error) {
	err := o.system.Shutdown(o.ctx)
	return err == nil, err
}

// forceStop deliberately does NOT reuse o.ctx: stopMachine calls forceStop
// as the floor even when o.ctx already hit its deadline during the grace
// wait (that's the whole point of runAsync there), so forceStop needs its
// own fresh window — reusing an already-expired ctx here would make
// Terminate report failure on ctx.Err() alone, even on a request that
// actually reached HCS, breaking "force is the floor" for exactly the
// caller-timeout case it exists to cover. vzMachine's forceStop sidesteps
// this the same way by taking no context at all.
func (o hcsStopOps) forceStop() error {
	ctx, cancel := bounded.WithTimeout(context.Background(), forceStopTimeout)
	defer cancel()
	return o.system.Terminate(ctx)
}
