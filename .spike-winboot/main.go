//go:build windows

// Phase-1 soak harness: create N differencing children off a Windows parent
// VHDX, boot them CONCURRENTLY on runny's HCS backend (Windows Secure Boot
// template), and per clone record: the HNS pre-commit IP, whether a forced
// layer-2 ARP confirms the guest actually lives at that IP (divergence test),
// and boot-to-networked time. Answers the blind-observable half of Spike C +
// folds in the Spike-B divergence soak:
//   - does the differencing-clone model work for a Windows parent? (N children
//     boot concurrently without corruption)
//   - does each clone get a distinct IP? (network-identity uniqueness)
//   - divergence rate across N boots (HNS pre-commit vs ARP-confirmed)
//   - boot magnitude for the per-OS FSM bounds
// Throwaway; not shipped. The SID/COMPUTERNAME-collision verdict needs guest
// login (a provisioned image) and is out of scope here.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	vhd "github.com/Microsoft/go-winio/vhd"
	"github.com/bojanrajkovic/runny/internal/winhcs/hcn"
	"github.com/bojanrajkovic/runny/internal/winhcs/hcs"
	hcsschema "github.com/bojanrajkovic/runny/internal/winhcs/hcs/schema2"
)

const (
	windowsSecureBootTemplateID = "1734c6e8-3154-4dda-ba5f-a874cc483422"
	defaultSwitchName           = "Default Switch"
)

type result struct {
	i           int
	mac         string
	preCommitIP string
	arpIP       string // IP the guest actually answered ARP from (empty if none)
	bootDur     time.Duration
	err         error
}

func main() {
	if len(os.Args) < 3 {
		fmt.Println("usage: winboot <parent-vhdx> <n>")
		os.Exit(2)
	}
	parent := os.Args[1]
	n, _ := strconv.Atoi(os.Args[2])
	if n < 1 {
		n = 1
	}

	network, err := hcn.GetNetworkByName(defaultSwitchName)
	if err != nil {
		fmt.Printf("RESULT: GetNetworkByName(%q) FAILED: %v\n", defaultSwitchName, err)
		os.Exit(1)
	}

	fmt.Printf("booting %d concurrent differencing clones off %s\n", n, parent)
	results := make([]result, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = runClone(network, parent, i)
		}(i)
	}
	wg.Wait()

	fmt.Println("\n==================== RESULTS ====================")
	seen := map[string]int{}
	var booted, diverged, distinct int
	for _, r := range results {
		if r.err != nil {
			fmt.Printf("clone %d: ERROR: %v\n", r.i, r.err)
			continue
		}
		div := "-"
		switch {
		case r.arpIP == "":
			div = "NO-ARP (guest not confirmed at pre-commit IP; maybe still in OOBE, or diverged)"
		case r.arpIP != r.preCommitIP:
			div = fmt.Sprintf("DIVERGED (pre-commit %s != real %s)", r.preCommitIP, r.arpIP)
			diverged++
		default:
			div = "no divergence (ARP-confirmed at pre-commit IP)"
			booted++
		}
		if r.arpIP != "" {
			seen[r.arpIP]++
		}
		fmt.Printf("clone %d: mac=%s preCommit=%s arpIP=%s boot=%s  %s\n",
			r.i, r.mac, r.preCommitIP, r.arpIP, r.bootDur.Round(time.Second), div)
	}
	for _, c := range seen {
		if c == 1 {
			distinct++
		}
	}
	fmt.Println("------------------------------------------------")
	fmt.Printf("clones ARP-confirmed at their pre-commit IP: %d/%d\n", booted, n)
	fmt.Printf("divergences (real IP != pre-commit): %d\n", diverged)
	fmt.Printf("distinct confirmed IPs: %d (of %d confirmed) -> %s\n",
		distinct, len(seen), map[bool]string{true: "all unique", false: "COLLISION"}[distinct == len(seen) && len(seen) > 0])
	fmt.Println("cleanup: terminating systems, deleting endpoints + clone disks (deferred)...")
}

func runClone(network *hcn.HostComputeNetwork, parent string, i int) result {
	r := result{i: i}
	child := fmt.Sprintf(`C:\Temp\winspike-clone-%d.vhdx`, i)
	_ = os.Remove(child)
	if err := vhd.CreateDiffVhd(child, parent, 0); err != nil {
		r.err = fmt.Errorf("CreateDiffVhd: %w", err)
		return r
	}
	defer os.Remove(child)

	ep, err := network.CreateEndpoint(&hcn.HostComputeEndpoint{
		Name:          fmt.Sprintf("runny-winspike-%d-ep", i),
		SchemaVersion: hcn.V2SchemaVersion(),
	})
	if err != nil {
		r.err = fmt.Errorf("CreateEndpoint: %w", err)
		return r
	}
	defer func() { _ = ep.Delete() }()
	r.mac = ep.MacAddress

	doc := &hcsschema.ComputeSystem{
		Owner:         "runny-spike",
		SchemaVersion: &hcsschema.Version{Major: 2, Minor: 1},
		VirtualMachine: &hcsschema.VirtualMachine{
			Chipset: &hcsschema.Chipset{Uefi: &hcsschema.Uefi{
				ApplySecureBootTemplate: "Apply",
				SecureBootTemplateId:    windowsSecureBootTemplateID,
			}},
			ComputeTopology: &hcsschema.Topology{
				Memory:    &hcsschema.VirtualMachineMemory{SizeInMB: 4096},
				Processor: &hcsschema.VirtualMachineProcessor{Count: 2},
			},
			Devices: &hcsschema.Devices{
				HvSocket: &hcsschema.HvSocket2{HvSocketConfig: &hcsschema.HvSocketSystemConfig{}},
				Scsi: map[string]hcsschema.Scsi{
					"0": {Attachments: map[string]hcsschema.Attachment{
						"0": {Type_: "VirtualDisk", Path: child},
					}},
				},
				ComPorts: map[string]hcsschema.ComPort{
					"0": {NamedPipe: fmt.Sprintf(`\\.\pipe\runny-winspike-%d`, i)},
				},
				NetworkAdapters: map[string]hcsschema.NetworkAdapter{
					"0": {EndpointId: ep.Id, MacAddress: ep.MacAddress},
				},
			},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	system, err := hcs.CreateComputeSystem(ctx, fmt.Sprintf("runny-winspike-%d", i), doc)
	if err != nil {
		r.err = fmt.Errorf("CreateComputeSystem: %w", err)
		return r
	}
	defer func() { _ = system.Terminate(context.Background()) }()
	start := time.Now()
	if err := system.Start(ctx); err != nil {
		r.err = fmt.Errorf("Start: %w", err)
		return r
	}

	for t := 0; t < 72; t++ { // up to 6 min
		time.Sleep(5 * time.Second)
		ip := neighborIP(ep.MacAddress)
		if ip == "" {
			continue
		}
		if r.preCommitIP == "" {
			r.preCommitIP = ip
		}
		if aip := arpConfirm(ep.MacAddress, ip); aip != "" {
			r.arpIP = aip
			r.bootDur = time.Since(start)
			return r
		}
	}
	return r
}

// neighborIP returns the first non-zero, non-link-local IPv4 the host neighbor
// table holds for mac (the pre-commit entry).
func neighborIP(mac string) string {
	ps := fmt.Sprintf(`$n = Get-NetNeighbor -LinkLayerAddress '%s' -AddressFamily IPv4 -ErrorAction SilentlyContinue | Where-Object { $_.IPAddress -notmatch '^(0\.|169\.254\.)' } | Select-Object -First 1; if ($n) { $n.IPAddress }`, mac)
	out, _ := exec.Command("powershell", "-NoProfile", "-Command", ps).Output()
	return strings.TrimSpace(string(out))
}

// arpConfirm deletes the static pre-commit for mac, forces a real ARP to ip
// (layer 2, unfirewalled), and returns the IP the mac dynamically resolved to
// (== ip means the guest is really there; "" means no ARP reply).
func arpConfirm(mac, ip string) string {
	macLow := strings.ToLower(mac)
	ps := fmt.Sprintf(`Get-NetNeighbor -LinkLayerAddress '%s' -AddressFamily IPv4 -ErrorAction SilentlyContinue | Remove-NetNeighbor -Confirm:$false -ErrorAction SilentlyContinue; Test-Connection -ComputerName '%s' -Count 2 -Quiet -ErrorAction SilentlyContinue | Out-Null; Start-Sleep -Seconds 2; arp -a`, mac, ip)
	out, _ := exec.Command("powershell", "-NoProfile", "-Command", ps).Output()
	for _, line := range strings.Split(string(out), "\n") {
		l := strings.ToLower(line)
		if strings.Contains(l, macLow) && strings.Contains(l, "dynamic") {
			if f := strings.Fields(line); len(f) >= 1 {
				return f[0]
			}
		}
	}
	return ""
}
