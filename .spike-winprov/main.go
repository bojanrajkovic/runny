//go:build windows

// Provision-boot harness: boot a pristine WS2025 eval VHDX (with unattend.xml +
// SetupComplete.cmd already injected) DIRECTLY (read-write, so OOBE's changes
// persist into the disk), wait for OpenSSH to come up, SSH in over x/crypto/ssh
// (matching runny's own bootstrap: baked password + InsecureIgnoreHostKey),
// run the baseline Spike-C checks (machine SID via Administrator's SID, and
// COMPUTERNAME), then seal the disk with a graceful shutdown. The sealed VHDX
// becomes the provisioned parent for the multi-clone Spike-C. Throwaway.
package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/bojanrajkovic/runny/internal/winhcs/hcn"
	"github.com/bojanrajkovic/runny/internal/winhcs/hcs"
	hcsschema "github.com/bojanrajkovic/runny/internal/winhcs/hcs/schema2"
	"golang.org/x/crypto/ssh"
)

const (
	windowsSecureBootTemplateID = "1734c6e8-3154-4dda-ba5f-a874cc483422"
	defaultSwitchName           = "Default Switch"
	guestUser                   = "Administrator"
	guestPass                   = "Runny-Spike-2026!"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: winprov <vhdx-path>")
		os.Exit(2)
	}
	vhd := os.Args[1]

	network, err := hcn.GetNetworkByName(defaultSwitchName)
	if err != nil {
		fmt.Println("RESULT: GetNetworkByName FAILED:", err)
		os.Exit(1)
	}
	ep, err := network.CreateEndpoint(&hcn.HostComputeEndpoint{
		Name: "runny-winprov-ep", SchemaVersion: hcn.V2SchemaVersion(),
	})
	if err != nil {
		fmt.Println("RESULT: CreateEndpoint FAILED:", err)
		os.Exit(1)
	}
	defer func() { _ = ep.Delete() }()
	fmt.Println("HNS endpoint MAC:", ep.MacAddress)

	doc := &hcsschema.ComputeSystem{
		Owner:         "runny-spike",
		SchemaVersion: &hcsschema.Version{Major: 2, Minor: 1},
		VirtualMachine: &hcsschema.VirtualMachine{
			Chipset: &hcsschema.Chipset{Uefi: &hcsschema.Uefi{
				ApplySecureBootTemplate: "Apply", SecureBootTemplateId: windowsSecureBootTemplateID,
			}},
			ComputeTopology: &hcsschema.Topology{
				Memory:    &hcsschema.VirtualMachineMemory{SizeInMB: 4096},
				Processor: &hcsschema.VirtualMachineProcessor{Count: 2},
			},
			Devices: &hcsschema.Devices{
				HvSocket: &hcsschema.HvSocket2{HvSocketConfig: &hcsschema.HvSocketSystemConfig{}},
				Scsi: map[string]hcsschema.Scsi{"0": {Attachments: map[string]hcsschema.Attachment{
					"0": {Type_: "VirtualDisk", Path: vhd},
				}}},
				ComPorts:        map[string]hcsschema.ComPort{"0": {NamedPipe: `\\.\pipe\runny-winprov`}},
				NetworkAdapters: map[string]hcsschema.NetworkAdapter{"0": {EndpointId: ep.Id, MacAddress: ep.MacAddress}},
			},
		},
	}

	ctx := context.Background()
	system, err := hcs.CreateComputeSystem(ctx, "runny-winprov", doc)
	if err != nil {
		fmt.Println("RESULT: CreateComputeSystem FAILED:", err)
		os.Exit(1)
	}
	bootStart := time.Now()
	if err := system.Start(ctx); err != nil {
		fmt.Println("RESULT: Start FAILED:", err)
		_ = system.Terminate(context.Background())
		os.Exit(1)
	}
	fmt.Println("STARTED. OOBE + SetupComplete (OpenSSH install) takes several minutes; polling for TCP 22...")

	var ip string
	deadline := time.Now().Add(20 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Second)
		if cand := neighborIP(ep.MacAddress); cand != "" {
			if ip == "" {
				ip = cand
				fmt.Printf("  neighbor IP %s (t+%s)\n", ip, time.Since(bootStart).Round(time.Second))
			}
			if dialable(ip, 22) {
				fmt.Printf(">>> SSH (22) UP at %s after %s\n", ip, time.Since(bootStart).Round(time.Second))
				break
			}
		}
	}
	if ip == "" || !dialable(ip, 22) {
		fmt.Println("RESULT: SSH never came up in 20min -> provisioning FAILED; terminating.")
		_ = system.Terminate(context.Background())
		os.Exit(1)
	}

	fmt.Println("--- SSH baseline checks (Administrator) ---")
	out, err := sshRun(ip, []string{"whoami /user", "hostname", "cmd /c ver"})
	if err != nil {
		fmt.Println("SSH session error:", err)
	} else {
		fmt.Println(out)
	}

	fmt.Println("--- sealing: graceful shutdown ---")
	if err == nil {
		_, _ = sshRun(ip, []string{"shutdown /s /t 3 /f"})
	} else {
		_ = system.Shutdown(context.Background())
	}
	waitCh := make(chan struct{})
	go func() { _ = system.Wait(); close(waitCh) }()
	select {
	case <-waitCh:
		fmt.Printf("RESULT: PASS. guest powered off -> VHDX sealed as provisioned parent. SSH bring-up confirmed at %s.\n", ip)
	case <-time.After(6 * time.Minute):
		fmt.Println("RESULT: timeout waiting for poweroff; force-terminating (disk may be unclean).")
		_ = system.Terminate(context.Background())
	}
}

func sshRun(ip string, cmds []string) (string, error) {
	cfg := &ssh.ClientConfig{
		User:            guestUser,
		Auth:            []ssh.AuthMethod{ssh.Password(guestPass)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         15 * time.Second,
	}
	client, err := ssh.Dial("tcp", net.JoinHostPort(ip, "22"), cfg)
	if err != nil {
		return "", err
	}
	defer client.Close()
	var b strings.Builder
	for _, c := range cmds {
		sess, err := client.NewSession()
		if err != nil {
			return b.String(), err
		}
		o, _ := sess.CombinedOutput(c)
		fmt.Fprintf(&b, "$ %s\n%s\n", c, strings.TrimRight(string(o), "\r\n"))
		_ = sess.Close()
	}
	return b.String(), nil
}

func neighborIP(mac string) string {
	ps := fmt.Sprintf(`$n = Get-NetNeighbor -LinkLayerAddress '%s' -AddressFamily IPv4 -ErrorAction SilentlyContinue | Where-Object { $_.IPAddress -notmatch '^(0\.|169\.254\.)' } | Select-Object -First 1; if ($n) { $n.IPAddress }`, mac)
	out, _ := exec.Command("powershell", "-NoProfile", "-Command", ps).Output()
	return strings.TrimSpace(string(out))
}

func dialable(ip string, port int) bool {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", ip, port), 3*time.Second)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}
