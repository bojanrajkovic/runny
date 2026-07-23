//go:build windows

package vm

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"path/filepath"
	"time"

	"github.com/bojanrajkovic/runny/internal/bounded"
	"github.com/bojanrajkovic/runny/internal/obs"
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
	if err := reapPriorSystem(hcsReapOps{}, systemID, endpointName); err != nil {
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

	return &hcsMachine{
		system:      system,
		endpoint:    ep,
		mac:         ep.MacAddress,
		systemID:    systemID,
		sshUser:     opts.SSHUser,
		sshPassword: opts.SSHPassword,
	}, nil
}

// consolePipeName is per-system so concurrent slots never collide on the
// same named pipe. hcsMachine.WaitIP's fixupNetwork fallback dials it when
// the guest doesn't self-configure networking within waitIPGracePeriod (see
// WaitIP's doc comment); it also exists for operator/debug access to the
// boot console, the same reason vz guests get a display even headless.
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

// scrubNeighborEntry removes the stale Permanent neighbor-table entries HNS
// leaves behind for mac once its endpoint is gone — see hcsMachine.destroy's
// doc comment for why this doesn't happen on its own. Shared by every
// cleanup path that deletes an endpoint (a boot that fails at Start, and a
// confirmed-stopped Machine both attach the endpoint before either one is
// reached, so both can leak the same entries). A divergent boot leaves MORE
// THAN ONE Permanent row for the MAC (stale pre-commit plus real lease), so
// this deletes every match, not just the first — otherwise the leftover
// accumulates one row per diverged boot. Best-effort: a delete failure is
// logged and the loop continues (one failure must not skip the rest), since
// by this point the endpoint is already gone and there's nothing further a
// caller could do differently.
func scrubNeighborEntry(mac string) {
	entries, err := readNeighborEntries()
	if err != nil {
		slog.Error("teardown: reading neighbor table for stale-entry scrub failed", "mac", mac, "err", err)
		return
	}
	for _, e := range permanentEntriesForMAC(entries, mac) {
		if err := deleteNeighborEntry(e.ip, e.interfaceIndex); err != nil {
			slog.Error("teardown: stale neighbor-table entry delete failed", "ip", e.ip, "mac", mac, "err", err)
		}
	}
}

// reapPriorSystem is defined in reap_windows.go, alongside the reapOps seam
// that makes its decision logic unit-testable off real hardware.

// abandonComputeSystem is the failed-Start cleanup: nothing owns system or
// ep once Boot returns an error (the caller never got a Machine), so Boot
// must tear them down itself here or they leak — the same class of orphan
// this backend's own hardware spike found accumulating from earlier manual
// runs (issue #308). The endpoint is already wired into the compute-system
// document, and CreateComputeSystem already succeeded, before Start is ever
// called — so a Start that fails can still leak the same stale
// neighbor-table entry a confirmed-stopped Machine's destroy() scrubs;
// scrubNeighborEntry is shared for exactly that reason. terminateAndClose
// (reap_windows.go, shared with reapPriorSystem) waits for the real exit
// notification before closing the handle or deleting the endpoint:
// closing/deleting out from under a guest that might still be running is
// the mistake hcsMachine.Stop's wedged-stop path already avoids. A wait
// that doesn't resolve in time leaves system/ep untouched for this slot's
// next Boot (and its own reapPriorSystem) to try again, rather than risk
// that.
func abandonComputeSystem(system *hcs.System, ep *hcn.HostComputeEndpoint) {
	ctx, cancel := bounded.WithTimeout(context.Background(), abandonedStopTimeout)
	defer cancel()
	if err := terminateAndClose(ctx, system, "abandoned compute system"); err != nil {
		slog.Error("abandoned compute system: did not exit before the cleanup window closed; leaving it in place for a later reap", "id", system.ID(), "err", err)
		return
	}
	deleteEndpointAndScrub(hcnEndpoint{ep}, "abandoned HNS endpoint")
}

type hcsMachine struct {
	system   *hcs.System
	endpoint *hcn.HostComputeEndpoint
	mac      string
	systemID string

	// sshUser/sshPassword are only used by WaitIP's fixupNetwork fallback --
	// Boot itself needs neither, see WaitIP's doc comment.
	sshUser     string
	sshPassword string
}

func (m *hcsMachine) MAC() string { return m.mac }

// NeedsRunnerPush is always true here: schema 2.1's only Linux-guest-capable
// share device is Plan9, and hot-adding a Plan9 share to a bare (non-LCOW)
// compute system is rejected by HCS outright -- confirmed against real
// hardware (issue #319): a generic Modify (a benign memory-size update)
// succeeds fine on the same bare compute system, so Modify itself isn't the
// problem -- Plan9 specifically is paired with LCOW's own guest-side GCS
// bridge control protocol, not a device the guest kernel discovers on its
// own the way SCSI/NetworkAdapters are. Boot never attaches anything for
// RunnerShareDir, so PROVISION must push the tarball itself.
func (m *hcsMachine) NeedsRunnerPush() bool { return true }

// waitIPGracePeriod is how long WaitIP trusts the guest to self-configure
// networking before it assumes the hv_netvsc/netplan mismatch fixupNetwork
// exists for (issue #319, confirmed on the currently-validated image) and
// falls back to it. Sized off HNS's own binding behavior -- the neighbor-
// table entry it programs appears "within seconds of Start" for a guest
// that completes DHCP on its own -- times a generous margin; a future image
// that really does self-configure only ever pays this once, on its first
// poll past a normal DHCP round-trip.
const waitIPGracePeriod = 10 * time.Second

// WaitIP returns the guest's actual lease IP -- the address later states dial
// for SSH, and the one stamped as vm.ip in telemetry.
//
// Within the grace period it accepts only a LEARNED neighbor row
// (Reachable/Stale via learnedLeaseIP) -- an actual ARP resolution proving the
// guest self-configured and is reachable. It never accepts HNS's Permanent row:
// that's a pre-boot pre-commit, a guess the guest's DHCP routinely overrides, so
// returning it would dial a stale IP and, worse, short-circuit before the fixup
// that would have corrected it. A guest with a genuinely good netplan resolves
// to a learned row and WaitIP returns within grace.
//
// The currently-validated guest image (ghcr.io/cirruslabs/ubuntu-runner-amd64)
// does NOT self-configure: its baked netplan matches interface names "en*",
// which hv_netvsc's always-"eth0" naming never satisfies, so eth0 sits down and
// DHCP never starts without fixupNetwork's console-applied drop-in (issue #319).
// On this host HNS surfaces no learned row at all (every row is Permanent), so
// grace always elapses to the fixup. Once the fixup has logged in, the address
// eth0 actually holds is read straight off the console and returned as
// authoritative -- the neighbor table is only re-read to detect, and record,
// the divergence against any stale Permanent pre-commit.
func (m *hcsMachine) WaitIP(ctx bounded.Context) (string, error) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	deadline := boundedDeadline(ctx, waitIPGracePeriod)
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
			if ip, ok := learnedLeaseIP(entries, m.mac); ok {
				return ip, nil
			}
			if time.Now().Before(deadline) {
				continue // still within grace; let the guest self-configure
			}
			if m.sshUser == "" || m.sshPassword == "" {
				return "", fmt.Errorf("no IP after %s and no SSHUser/SSHPassword configured to attempt the network fixup (issue #319)", waitIPGracePeriod)
			}
			// Wrapped in its own named action (not folded silently into
			// AWAIT_IP's step span) so a trace/metric can distinguish "this
			// cycle needed the fixup" from "HNS was just slow" -- the two
			// looked identical before, with an AWAIT_SSH failure downstream
			// of a WaitIP that had just reported as good, and nothing
			// correlating it back to the fixup-fallback path.
			var leaseIP string
			if err := obs.Action(ctx, obs.ActionNetworkFixup, func(context.Context) error {
				obs.Milestone(ctx, "grace-period-elapsed")
				ip, err := fixupNetwork(ctx, m.systemID, m.sshUser, m.sshPassword)
				if err != nil {
					return err
				}
				leaseIP = ip
				// Re-read AFTER the fixup: by now the guest's DHCP has settled
				// and any stale HNS pre-commit rows for this MAC are visible, so
				// this is the snapshot that actually shows the divergence the
				// pre-fixup one couldn't (a fresh guest has no pre-commit row
				// yet at grace-elapse). Best-effort: a transient re-read failure
				// just skips the diagnostic, never fails the cycle.
				if post, err := readNeighborEntries(); err == nil {
					if other := divergentPermanentIPs(post, m.mac, leaseIP); len(other) > 0 {
						// no-silent-failure: the milestone flags that WaitIP
						// dialed something the neighbor table disagrees with, and
						// the log carries the stale rows for the operator asking why.
						obs.Milestone(ctx, "neighbor-ip-corrected")
						slog.Warn("WaitIP: neighbor table holds a stale pre-commit differing from the guest's real lease; dialing the console-observed address",
							"mac", m.mac, "neighbor_ips", other, "lease_ip", leaseIP)
					}
				}
				return nil
			}); err != nil {
				return "", fmt.Errorf("network fixup: %w", err)
			}
			return leaseIP, nil
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
