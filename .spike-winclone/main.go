//go:build windows

// Multi-clone Spike-C: create N differencing children off the SEALED provisioned
// parent (SSH already enabled), boot them CONCURRENTLY, SSH into each, and
// collect COMPUTERNAME + machine SID + NetBIOS state. Since the parent is
// un-generalized (no sysprep), every clone inherits the SAME name and SID --
// the decisive skip-sysprep test: do byte-identical duplicate-identity clones
// all run and stay reachable, or do they collide (NetBIOS/DNS/networking)?
// Clones are throwaway, so force-terminate after collecting. Throwaway harness.
package main

import (
	"context"
	"fmt"
	"net"
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
	"golang.org/x/crypto/ssh"
)

const (
	windowsSecureBootTemplateID = "1734c6e8-3154-4dda-ba5f-a874cc483422"
	defaultSwitchName           = "Default Switch"
	guestUser                   = "Administrator"
	guestPass                   = "Runny-Spike-2026!"
)

type result struct {
	i        int
	ip       string
	hostname string
	sid      string // machine SID (Administrator SID minus -500)
	nbt      string // one-line NetBIOS summary
	sshOK    bool
	bootDur  time.Duration
	err      error
}

func main() {
	if len(os.Args) < 3 {
		fmt.Println("usage: winclone <sealed-parent-vhdx> <n>")
		os.Exit(2)
	}
	parent := os.Args[1]
	n, _ := strconv.Atoi(os.Args[2])
	if n < 1 {
		n = 1
	}
	network, err := hcn.GetNetworkByName(defaultSwitchName)
	if err != nil {
		fmt.Println("RESULT: GetNetworkByName FAILED:", err)
		os.Exit(1)
	}

	fmt.Printf("booting %d concurrent differencing clones off sealed parent %s\n", n, parent)
	results := make([]result, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); results[i] = runClone(network, parent, i) }(i)
	}
	wg.Wait()

	fmt.Println("\n==================== RESULTS ====================")
	hosts := map[string]int{}
	sids := map[string]int{}
	ips := map[string]int{}
	sshUp := 0
	for _, r := range results {
		if r.err != nil {
			fmt.Printf("clone %d: ERROR: %v\n", r.i, r.err)
			continue
		}
		fmt.Printf("clone %d: ip=%s host=%s sid=%s boot=%s ssh=%v\n  nbt: %s\n",
			r.i, r.ip, r.hostname, r.sid, r.bootDur.Round(time.Second), r.sshOK, r.nbt)
		if r.sshOK {
			sshUp++
			hosts[r.hostname]++
			sids[r.sid]++
		}
		if r.ip != "" {
			ips[r.ip]++
		}
	}
	fmt.Println("------------------------------------------------")
	fmt.Printf("SSH-reachable clones: %d/%d\n", sshUp, n)
	fmt.Printf("distinct COMPUTERNAMEs: %d (expect 1 -> all duplicate, un-generalized)\n", len(hosts))
	fmt.Printf("distinct machine SIDs: %d (expect 1 -> all duplicate)\n", len(sids))
	fmt.Printf("distinct IPs: %d/%d (expect all-unique -> no network collision)\n", len(ips), n)
	fmt.Println("VERDICT: if SSH-reachable == n AND distinct IPs == n, duplicate name/SID clones coexist fine")
	fmt.Println("         (skip-sysprep holds); a NetBIOS 'CONFLICT' in the nbt lines would be the one caveat.")
	fmt.Println("cleanup: force-terminating clones, deleting endpoints + disks (deferred)...")
}

func runClone(network *hcn.HostComputeNetwork, parent string, i int) result {
	r := result{i: i}
	child := fmt.Sprintf(`C:\Temp\winclone-%d.vhdx`, i)
	_ = os.Remove(child)
	if err := vhd.CreateDiffVhd(child, parent, 0); err != nil {
		r.err = fmt.Errorf("CreateDiffVhd: %w", err)
		return r
	}
	defer os.Remove(child)

	ep, err := network.CreateEndpoint(&hcn.HostComputeEndpoint{
		Name: fmt.Sprintf("runny-winclone-%d-ep", i), SchemaVersion: hcn.V2SchemaVersion(),
	})
	if err != nil {
		r.err = fmt.Errorf("CreateEndpoint: %w", err)
		return r
	}
	defer func() { _ = ep.Delete() }()

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
					"0": {Type_: "VirtualDisk", Path: child},
				}}},
				ComPorts:        map[string]hcsschema.ComPort{"0": {NamedPipe: fmt.Sprintf(`\\.\pipe\runny-winclone-%d`, i)}},
				NetworkAdapters: map[string]hcsschema.NetworkAdapter{"0": {EndpointId: ep.Id, MacAddress: ep.MacAddress}},
			},
		},
	}
	ctx := context.Background()
	system, err := hcs.CreateComputeSystem(ctx, fmt.Sprintf("runny-winclone-%d", i), doc)
	if err != nil {
		r.err = fmt.Errorf("CreateComputeSystem: %w", err)
		return r
	}
	defer func() { _ = system.Terminate(context.Background()) }() // throwaway clone: force-terminate is fine
	start := time.Now()
	if err := system.Start(ctx); err != nil {
		r.err = fmt.Errorf("Start: %w", err)
		return r
	}

	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Second)
		ip := neighborIP(ep.MacAddress)
		if ip == "" || !dialable(ip, 22) {
			if ip != "" && r.ip == "" {
				r.ip = ip
			}
			continue
		}
		r.ip = ip
		r.bootDur = time.Since(start)
		out, serr := sshRun(ip, []string{"hostname", "whoami /user", "nbtstat -n"})
		if serr != nil {
			r.err = fmt.Errorf("ssh: %w", serr)
			return r
		}
		r.sshOK = true
		r.hostname, r.sid, r.nbt = parseChecks(out)
		return r
	}
	r.err = fmt.Errorf("SSH never came up within 10min (last ip %q)", r.ip)
	return r
}

// parseChecks pulls COMPUTERNAME, machine SID (Administrator SID trimmed of the
// -500 RID), and a NetBIOS summary out of the combined command output.
func parseChecks(out string) (host, sid, nbt string) {
	lines := strings.Split(out, "\n")
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(strings.ToLower(t), "$ hostname") && i+1 < len(lines) {
			host = strings.TrimSpace(lines[i+1])
		}
		if strings.Contains(strings.ToUpper(t), "\\ADMINISTRATOR") && strings.Contains(t, "S-1-5-21") {
			for _, f := range strings.Fields(t) {
				if strings.HasPrefix(f, "S-1-5-21-") {
					sid = strings.TrimSuffix(f, "-500")
				}
			}
		}
	}
	conflict := "no NetBIOS conflict flagged"
	if strings.Contains(strings.ToUpper(out), "CONFLICT") {
		conflict = "NetBIOS CONFLICT present"
	}
	// grab the registered <00>/<20> names line count as a crude health signal
	reg := strings.Count(strings.ToUpper(out), "REGISTERED")
	nbt = fmt.Sprintf("%s; %d registered NetBIOS name(s)", conflict, reg)
	return host, sid, nbt
}

func sshRun(ip string, cmds []string) (string, error) {
	cfg := &ssh.ClientConfig{
		User: guestUser, Auth: []ssh.AuthMethod{ssh.Password(guestPass)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 15 * time.Second,
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
