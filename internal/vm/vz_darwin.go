//go:build darwin

package vm

import (
	"context"
	"fmt"
	"os"
	"time"

	vz "github.com/Code-Hex/vz/v3"

	"github.com/bojanrajkovic/runny/internal/tart"
)

// VZManager is the real Manager: in-process Virtualization.framework via
// Code-Hex/vz. The binary must be codesigned with the
// com.apple.security.virtualization entitlement (ADR-0008).
type VZManager struct{}

var _ Manager = VZManager{}

// Boot builds the VZ configuration from the bundle's tart config and starts
// the guest. A fresh machine identifier and a fresh random MAC are used —
// the bundle's own values may be shared by other clones (spike-verified).
func (VZManager) Boot(ctx context.Context, bundle tart.Bundle, opts BootOptions) (Machine, error) {
	cfg, err := bundle.LoadConfig()
	if err != nil {
		return nil, err
	}

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
	machineID, err := vz.NewMacMachineIdentifier()
	if err != nil {
		return nil, fmt.Errorf("machine identifier: %w", err)
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
	vmc, err := vz.NewVirtualMachineConfiguration(boot, cfg.CPUCount, cfg.MemorySize)
	if err != nil {
		return nil, err
	}
	vmc.SetPlatformVirtualMachineConfiguration(platform)

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

	if opts.RunnerCacheDir != "" {
		fs, err := vz.NewVirtioFileSystemDeviceConfiguration(ShareTag)
		if err != nil {
			return nil, fmt.Errorf("virtiofs device: %w", err)
		}
		dir, err := vz.NewSharedDirectory(opts.RunnerCacheDir, true)
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
	if err := machine.Start(); err != nil {
		return nil, fmt.Errorf("vm start: %w", err)
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

func (m *vzMachine) MAC() string           { return m.mac }
func (m *vzMachine) Done() <-chan struct{} { return m.done }

func (m *vzMachine) watchState() {
	defer close(m.done)
	for state := range m.vm.StateChangedNotify() {
		switch state {
		case vz.VirtualMachineStateStopped, vz.VirtualMachineStateError:
			return
		}
	}
}

func (m *vzMachine) WaitIP(ctx context.Context) (string, error) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("waiting for DHCP lease for %s: %w", m.mac, ctx.Err())
		case <-m.done:
			return "", fmt.Errorf("guest stopped while waiting for IP")
		case <-t.C:
			data, err := os.ReadFile(LeasesPath)
			if err != nil {
				continue // file appears with the first lease
			}
			if ip, ok := FindIPByMAC(string(data), m.mac); ok {
				return ip, nil
			}
		}
	}
}

// Stop: RequestStop (graceful, often stalls on vanilla images — spike-
// verified) bounded by grace, then force Stop(). Force is the floor; this
// method only errors if even force-stop failed AND the guest still runs.
func (m *vzMachine) Stop(ctx context.Context, grace time.Duration) error {
	select {
	case <-m.done:
		return nil // already stopped
	default:
	}
	if ok, err := m.vm.RequestStop(); ok && err == nil {
		select {
		case <-m.done:
			return nil
		case <-time.After(grace):
		case <-ctx.Done():
		}
	}
	if err := m.vm.Stop(); err != nil {
		select {
		case <-m.done:
			return nil
		default:
			return fmt.Errorf("force stop failed with guest still running: %w", err)
		}
	}
	select {
	case <-m.done:
		return nil
	case <-time.After(10 * time.Second):
		return fmt.Errorf("guest did not reach stopped state after force stop")
	case <-ctx.Done():
		return fmt.Errorf("guest stop: %w", context.Cause(ctx))
	}
}
