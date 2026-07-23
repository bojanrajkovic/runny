package vm

import "testing"

func TestParseInetIP(t *testing.T) {
	// The real shape of `ip -4 -o addr show eth0`, as it arrives over the
	// console (CRLF, the leading interface index, the /prefix, trailing
	// scope/flags), plus the command echo the getty reflects back first.
	const out = "ip -4 -o addr show eth0\r\n" +
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
