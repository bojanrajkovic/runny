//go:build darwin

package vm

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	vz "github.com/Code-Hex/vz/v3"

	"github.com/bojanrajkovic/runny/internal/bounded"
	"github.com/bojanrajkovic/runny/internal/tart"
)

// VZManager is the real Manager: in-process Virtualization.framework via
// Code-Hex/vz. The binary must be codesigned with the
// com.apple.security.virtualization entitlement.
type VZManager struct{}

var _ Manager = VZManager{}

// ReapOrphans is a no-op on darwin: Virtualization.framework has no orphan
// class the way bare HCS compute systems do (see hcs_windows.go's
// ReapOrphans) — a *vz.VirtualMachine releases on GC / process exit, and
// there is no separate OS-level object left behind that could hold a clone
// file open across a restart.
func (VZManager) ReapOrphans(string) error { return nil }

// Boot builds the VZ configuration from the bundle's tart config and starts
// the guest, dispatching on the bundle's OS: the Mac platform
// path for darwin, EFI for linux. checkHostArch (shared with hcs_windows.go)
// rejects a bundle whose Arch doesn't match this host's own runtime.GOARCH —
// LoadConfig's own validation is a portable shape check that deliberately
// doesn't know which host it's running on, so a linux/amd64 bundle (a valid
// shape for the Windows/HCS backend) would otherwise reach bootLinux
// unchecked on an arm64 Mac, which can't execute it.
func (m VZManager) Boot(ctx bounded.Context, bundle tart.Bundle, opts BootOptions) (Machine, error) {
	cfg, err := bundle.LoadConfig()
	if err != nil {
		return nil, err
	}
	if err := checkHostArch(cfg); err != nil {
		return nil, err
	}
	switch cfg.OS {
	case "linux":
		return m.bootLinux(ctx, bundle, cfg, opts)
	default:
		return m.bootDarwin(ctx, bundle, cfg, opts)
	}
}

func (VZManager) bootDarwin(ctx context.Context, bundle tart.Bundle, cfg *tart.Config, opts BootOptions) (Machine, error) {
	hwData, err := cfg.HardwareModel()
	if err != nil {
		return nil, err
	}
	hw, err := vz.NewMacHardwareModelWithData(hwData)
	if err != nil {
		return nil, fmt.Errorf("hardware model: %w", err)
	}
	if !hw.Supported() {
		return nil, fmt.Errorf("bundle hardware model unsupported on this host")
	}
	// Persisted identifier, never fresh — see tart.Config.ECID.
	ecidData, err := cfg.ECID()
	if err != nil {
		return nil, err
	}
	machineID, err := vz.NewMacMachineIdentifierWithData(ecidData)
	if err != nil {
		return nil, fmt.Errorf("machine identifier: %w", err)
	}
	// vz returns a nil identifier (with nil error) for invalid data; the
	// empty round-trip catches it here, attributed.
	if len(machineID.DataRepresentation()) == 0 {
		return nil, fmt.Errorf("machine identifier: bundle ecid is not a valid VZMacMachineIdentifier data representation")
	}
	aux, err := vz.NewMacAuxiliaryStorage(bundle.NVRAMPath())
	if err != nil {
		return nil, fmt.Errorf("auxiliary storage: %w", err)
	}
	platform, err := vz.NewMacPlatformConfiguration(
		vz.WithMacAuxiliaryStorage(aux),
		vz.WithMacHardwareModel(hw),
		vz.WithMacMachineIdentifier(machineID),
	)
	if err != nil {
		return nil, fmt.Errorf("platform: %w", err)
	}
	boot, err := vz.NewMacOSBootLoader()
	if err != nil {
		return nil, err
	}
	cpu, mem, err := resolveSizing(cfg, opts)
	if err != nil {
		return nil, err
	}
	vmc, err := vz.NewVirtualMachineConfiguration(boot, cpu, mem)
	if err != nil {
		return nil, err
	}
	vmc.SetPlatformVirtualMachineConfiguration(platform)

	// macOS guests want a display even headless; mirror the bundle's.
	w, h := cfg.Display.Width, cfg.Display.Height
	if w == 0 || h == 0 {
		w, h = 1024, 768
	}
	gfx, err := vz.NewMacGraphicsDeviceConfiguration()
	if err != nil {
		return nil, err
	}
	display, err := vz.NewMacGraphicsDisplayConfiguration(int64(w), int64(h), 80)
	if err != nil {
		return nil, err
	}
	gfx.SetDisplays(display)
	vmc.SetGraphicsDevicesVirtualMachineConfiguration([]vz.GraphicsDeviceConfiguration{gfx})

	return finishBoot(ctx, vmc, bundle, opts)
}

// bootLinux: EFI boot loader with the bundle's nvram.bin as the variable
// store, generic platform — the tart linux guest shape. No graphics needed.
func (VZManager) bootLinux(ctx context.Context, bundle tart.Bundle, cfg *tart.Config, opts BootOptions) (Machine, error) {
	efi, err := vz.NewEFIVariableStore(bundle.NVRAMPath())
	if err != nil {
		return nil, fmt.Errorf("efi variable store: %w", err)
	}
	boot, err := vz.NewEFIBootLoader(vz.WithEFIVariableStore(efi))
	if err != nil {
		return nil, fmt.Errorf("efi boot loader: %w", err)
	}
	cpu, mem, err := resolveSizing(cfg, opts)
	if err != nil {
		return nil, err
	}
	vmc, err := vz.NewVirtualMachineConfiguration(boot, cpu, mem)
	if err != nil {
		return nil, err
	}
	platform, err := vz.NewGenericPlatformConfiguration()
	if err != nil {
		return nil, fmt.Errorf("generic platform: %w", err)
	}
	vmc.SetPlatformVirtualMachineConfiguration(platform)
	return finishBoot(ctx, vmc, bundle, opts)
}

// abandonedStopTimeout bounds the detached best-effort force stop (below) of
// a machine whose boot blew its deadline. That path has no caller ctx left
// to bound it, so this is the only thing keeping its wait finite — and the
// signal that gets a wedged force stop logged rather than silently parked.
var abandonedStopTimeout = 30 * time.Second

// finishBoot attaches the OS-independent devices (disk, NAT net with a fresh
// MAC, the guest-agent console port + vsock, optional virtiofs cache
// share), validates, and starts the guest. The
// start itself honors ctx: Machine's contract says nothing here may block
// indefinitely, and the BOOT state's deadline only works if that is true —
// Start is an unbounded call into Virtualization.framework, the same
// framework whose graceful stop often stalls.
func finishBoot(ctx context.Context, vmc *vz.VirtualMachineConfiguration, bundle tart.Bundle, opts BootOptions) (Machine, error) {
	diskAtt, err := vz.NewDiskImageStorageDeviceAttachment(bundle.DiskPath(), false)
	if err != nil {
		return nil, fmt.Errorf("disk attachment: %w", err)
	}
	blk, err := vz.NewVirtioBlockDeviceConfiguration(diskAtt)
	if err != nil {
		return nil, err
	}
	vmc.SetStorageDevicesVirtualMachineConfiguration([]vz.StorageDeviceConfiguration{blk})

	mac, err := vz.NewRandomLocallyAdministeredMACAddress()
	if err != nil {
		return nil, err
	}
	natAtt, err := vz.NewNATNetworkDeviceAttachment()
	if err != nil {
		return nil, err
	}
	netDev, err := vz.NewVirtioNetworkDeviceConfiguration(natAtt)
	if err != nil {
		return nil, err
	}
	netDev.SetMACAddress(mac)
	vmc.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{netDev})

	// The cirruslabs images' tart-guest-agent SIGTERMs launchd on every
	// ~10 s respawn unless it finds this console port; macOS 15 guests then
	// cycle their userspace and the NIC flaps until SSH dies. This port
	// alone fixes it (bisect-verified); no other tart device matters.
	agentPort, err := vz.NewVirtioConsolePortConfiguration(
		vz.WithVirtioConsolePortConfigurationName(tart.GuestAgentPortName),
	)
	if err != nil {
		return nil, fmt.Errorf("guest-agent console port: %w", err)
	}
	console, err := vz.NewVirtioConsoleDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("console device: %w", err)
	}
	console.SetVirtioConsolePortConfiguration(0, agentPort)
	vmc.SetConsoleDevicesVirtualMachineConfiguration([]vz.ConsoleDeviceConfiguration{console})

	// vsock keeps the agent's RPC listener from retrying every second; it is
	// host-initiated only and runny never connects. No spice/clipboard port
	// — needless surface on a CI host.
	sock, err := vz.NewVirtioSocketDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("socket device: %w", err)
	}
	vmc.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{sock})

	if opts.RunnerShareDir != "" {
		fs, err := vz.NewVirtioFileSystemDeviceConfiguration(ShareTag)
		if err != nil {
			return nil, fmt.Errorf("virtiofs device: %w", err)
		}
		dir, err := vz.NewSharedDirectory(opts.RunnerShareDir, true)
		if err != nil {
			return nil, fmt.Errorf("shared directory: %w", err)
		}
		share, err := vz.NewSingleDirectoryShare(dir)
		if err != nil {
			return nil, fmt.Errorf("directory share: %w", err)
		}
		fs.SetDirectoryShare(share)
		vmc.SetDirectorySharingDevicesVirtualMachineConfiguration([]vz.DirectorySharingDeviceConfiguration{fs})
	}

	if ok, err := vmc.Validate(); !ok || err != nil {
		return nil, fmt.Errorf("vm config validate: ok=%v err=%w", ok, err)
	}
	machine, err := vz.NewVirtualMachine(vmc)
	if err != nil {
		return nil, err
	}
	started := runAsync(func() error { return machine.Start() })
	select {
	case err := <-started:
		if err != nil {
			return nil, fmt.Errorf("vm start: %w", err)
		}
	case <-ctx.Done():
		// The BOOT deadline fired mid-start. The machine was never returned,
		// so the FSM's teardown cannot own it — hand it a detached best-effort
		// force stop once Start finally returns. Best-effort is the floor here,
		// but never silent: the force stop is the same blocking cgo call the
		// teardown path bounds, so bound it the same way (runAsync) and log
		// whether it failed OR never returned — either way the guest may hold a
		// guest-cap slot until the daemon restarts.
		go func() {
			if err := <-started; err != nil {
				return // never started; nothing to stop
			}
			select {
			case serr := <-runAsync(machine.Stop):
				if serr != nil {
					slog.Error("abandoned boot: force stop failed; guest may hold a guest-cap slot until restart", "err", serr)
				}
			case <-time.After(abandonedStopTimeout):
				slog.Error("abandoned boot: force stop did not return; guest may hold a guest-cap slot until restart")
			}
		}()
		return nil, fmt.Errorf("vm start: %w", context.Cause(ctx))
	}

	m := &vzMachine{vm: machine, mac: mac.String(), done: make(chan struct{})}
	go m.watchState()
	return m, nil
}

type vzMachine struct {
	vm   *vz.VirtualMachine
	mac  string
	done chan struct{}
}

func (m *vzMachine) MAC() string { return m.mac }

// NeedsRunnerPush is always false here: Boot already attached RunnerShareDir
// as a live read-only virtiofs device (see finishBoot), so the tarball is
// already visible to the guest by the time PROVISION runs.
func (m *vzMachine) NeedsRunnerPush() bool { return false }

func (m *vzMachine) watchState() {
	defer close(m.done)
	for state := range m.vm.StateChangedNotify() {
		switch state {
		case vz.VirtualMachineStateStopped, vz.VirtualMachineStateError:
			return
		}
	}
}

func (m *vzMachine) WaitIP(ctx bounded.Context) (string, error) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("waiting for DHCP lease for %s: %w", m.mac, ctx.Err())
		case <-m.done:
			return "", fmt.Errorf("guest stopped while waiting for IP")
		case <-t.C:
			data, err := os.ReadFile(leasesPath)
			if err != nil {
				continue // file appears with the first lease
			}
			if ip, ok := FindIPByMAC(string(data), m.mac); ok {
				return ip, nil
			}
		}
	}
}

// Stop runs the bounded stop sequence (graceful RequestStop, then a force
// kill) shared by every platform; see stopMachine. RequestStop is graceful but
// often stalls on vanilla images (spike-verified), so force is the floor.
func (m *vzMachine) Stop(ctx bounded.Context, grace time.Duration) error {
	return stopMachine(ctx, grace, m.done, m)
}

// requestStop / forceStop are the cgo stop primitives stopMachine sequences.
// forceStop blocks until Virtualization.framework's completion handler fires,
// so stopMachine runs it off-goroutine and bounds the wait.
func (m *vzMachine) requestStop() (bool, error) { return m.vm.RequestStop() }
func (m *vzMachine) forceStop() error           { return m.vm.Stop() }
