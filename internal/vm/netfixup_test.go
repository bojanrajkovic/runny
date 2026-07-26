package vm

import (
	"strings"
	"testing"
)

func TestParseInetIP(t *testing.T) {
	// The whole console exchange as fixupNetwork actually drives it: the getty
	// echoes the real one-liner back (netplan drop-in + `ip -4 -o addr show
	// eth0`) before the command's own output arrives. The echoed command
	// contains no bare "inet " token, so the parser must not be fooled by it
	// and must pick the address off the real output line (CRLF, leading
	// interface index, /prefix, trailing scope/flags).
	const out = `printf 'network:\n  version: 2\n  ethernets:\n    eth0:\n      match:\n        driver: hv_netvsc\n      dhcp4: true\n' | sudo tee /etc/netplan/60-runny-hv-netvsc-fix.yaml >/dev/null && sudo netplan apply && sleep 5 && ip -4 -o addr show eth0` + "\r\n" +
		"2: eth0    inet 172.18.206.103/20 brd 172.18.207.255 scope global dynamic eth0\\       valid_lft 86400sec\r\n"
	if ip, ok := parseInetIP(out); !ok || ip != "172.18.206.103" {
		t.Errorf("parseInetIP = %q, %v; want 172.18.206.103", ip, ok)
	}

	// An IPv6 line must not be mistaken for the address: "inet6" is a distinct
	// token, so only the "inet " (v4) line is picked up.
	const v6first = "3: eth0    inet6 fe80::215:5dff:fe13:5930/64 scope link\r\n" +
		"2: eth0    inet 10.0.0.5/24 scope global\r\n"
	if ip, ok := parseInetIP(v6first); !ok || ip != "10.0.0.5" {
		t.Errorf("parseInetIP (v6 first) = %q, %v; want 10.0.0.5", ip, ok)
	}

	// No address yet (eth0 still down) -- the caller must be able to tell.
	if _, ok := parseInetIP("2: eth0    <NO-CARRIER> state DOWN\r\n"); ok {
		t.Error("parseInetIP reported an address where the output has none")
	}
	if _, ok := parseInetIP(""); ok {
		t.Error("parseInetIP reported an address from empty output")
	}
}

// TestConsolePipeNameIsUnpredictable pins the anti-squat property: the console
// pipe name must not be derivable from the slot alone. A name a local user can
// predict can be pre-created before the guest boots, and whoever creates it
// first owns its DACL -- which is how a squatter gets handed the console
// fixupNetwork types the guest's SSH credentials into.
func TestConsolePipeNameIsUnpredictable(t *testing.T) {
	const systemID = "pool-0"

	seen := make(map[string]bool, 64)
	for range 64 {
		name, err := consolePipeName(systemID)
		if err != nil {
			t.Fatalf("consolePipeName: %v", err)
		}
		if !strings.HasPrefix(name, `\\.\pipe\runny-console-`+systemID+"-") {
			t.Fatalf("name %q lost its identifying prefix; operators locate a guest's console by it", name)
		}
		if suffix := strings.TrimPrefix(name, `\\.\pipe\runny-console-`+systemID+"-"); len(suffix) < 16 {
			t.Errorf("suffix %q is too short to resist guessing", suffix)
		}
		if seen[name] {
			t.Fatalf("consolePipeName repeated %q — a repeatable name is a predictable one", name)
		}
		seen[name] = true
	}
}

// TestIsTrustedConsoleOwner pins the trust policy to SYSTEM alone. Hyper-V
// creates the console pipe as vmcompute.exe/SYSTEM (verified against a live
// guest), and SYSTEM is an owner an unprivileged squatter cannot forge.
func TestIsTrustedConsoleOwner(t *testing.T) {
	for _, tc := range []struct {
		name string
		sid  string
		want bool
	}{
		{"local system, what Hyper-V creates", "S-1-5-18", true},
		{"administrators is NOT enough for a console", "S-1-5-32-544", false},
		{"an arbitrary user", "S-1-5-21-1111111111-2222222222-3333333333-1001", false},
		{"a virtual machine account", "S-1-5-83-1-277479287-1468143679-1962191235-2497143080", false},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTrustedConsoleOwner(tc.sid); got != tc.want {
				t.Errorf("isTrustedConsoleOwner(%q) = %v, want %v", tc.sid, got, tc.want)
			}
		})
	}
}
