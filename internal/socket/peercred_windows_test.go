//go:build windows

package socket

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	winio "github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

func ownProcessSID(t *testing.T) string {
	t.Helper()
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatalf("GetTokenUser: %v", err)
	}
	return tu.User.Sid.String()
}

// TestReadPeerImpersonatesClientSID proves the windows identity read end to end
// over a real named pipe: the server impersonates the connecting client and
// recovers its SID, and the byte peeked at the handshake is replayed so the
// stream loses nothing. Self-connect suffices — it exercises
// impersonate → OpenThreadToken → GetTokenUser → RevertToSelf; the
// cross-principal property (reading a SID the unprivileged service account
// cannot reach via OpenProcess) was hardware-proven by the pipe spike and
// cannot be reproduced in a single process.
func TestReadPeerImpersonatesClientSID(t *testing.T) {
	name := uniquePipeName(t)
	ln, err := listen(name, true)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	type result struct {
		conn net.Conn
		id   string
		priv bool
		ok   bool
	}
	resCh := make(chan result, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			resCh <- result{}
			return
		}
		conn, id, priv, ok := readPeer(c)
		resCh <- result{conn, id, priv, ok}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := winio.DialPipeAccessImpLevel(ctx, name, pipeDialAccessTest, winio.PipeImpLevelIdentification)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = client.Close() }()
	// Bound the raw write: on a mis-sized (unbuffered) pipe a client write
	// rendezvous-blocks the server reader, so fail in seconds with a clear
	// message rather than hanging the suite until bazel's SIGKILL.
	_ = client.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := client.Write([]byte{'A', 'B'}); err != nil {
		t.Fatalf("client write: %v", err)
	}

	var res result
	select {
	case res = <-resCh:
	case <-time.After(5 * time.Second):
		t.Fatal("server readPeer did not return")
	}
	if !res.ok {
		t.Fatal("readPeer ok=false, want the client's SID read")
	}
	if want := ownProcessSID(t); res.id != want {
		t.Errorf("impersonated SID = %q, want this process's own SID %q", res.id, want)
	}
	// The peeked byte must be replayed: a full read of the returned conn sees
	// 'A' (peeked) then 'B'.
	_ = res.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	got := make([]byte, 2)
	if _, err := io.ReadFull(res.conn, got); err != nil {
		t.Fatalf("read replayed bytes: %v", err)
	}
	if string(got) != "AB" {
		t.Errorf("replayed stream = %q, want %q", got, "AB")
	}
}

// TestIsPrivilegedToken pins the privileged verdict logic runner-agnostically:
// SYSTEM's SID is privileged by the string short-circuit (the token is never
// consulted), and a non-SYSTEM identity's verdict must equal the token's actual
// Administrators membership. That membership is runner-dependent (true on an
// elevated-admin runner like CI's windows-2022, false for a plain user), and
// there is no deterministic way to force false from the runner's own token —
// CheckTokenMembership(NULL) duplicates the primary token and checks that — so
// we assert the verdict MATCHES an independent membership check on the same
// token rather than hardcoding either boolean. Both sides derive from the same
// IsMember call, so this pins the delegation without assuming privilege level.
func TestIsPrivilegedToken(t *testing.T) {
	if !isPrivilegedToken(systemSID, windows.Token(0)) {
		t.Error("SYSTEM SID must be privileged")
	}
	tok := windows.GetCurrentProcessToken()
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatalf("CreateWellKnownSid: %v", err)
	}
	member, merr := tok.IsMember(admins)
	want := merr == nil && member // exactly isPrivilegedToken's non-SYSTEM verdict
	if got := isPrivilegedToken("S-1-5-21-1-2-3-1001", tok); got != want {
		t.Errorf("non-SYSTEM verdict = %v, want %v (must match the token's admin membership)", got, want)
	}
}
