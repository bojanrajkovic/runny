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

// TestIsPrivilegedToken pins the privileged verdict logic deterministically,
// independent of whether the CI runner is elevated: SYSTEM's SID is privileged
// by the string short-circuit (the token is never consulted), and a non-SYSTEM
// SID checked against a primary (non-impersonation) token reads non-privileged
// — CheckTokenMembership rejects a primary token, and the code errs that to
// false (the require-an-explicit-ACE safe direction).
func TestIsPrivilegedToken(t *testing.T) {
	if !isPrivilegedToken(systemSID, windows.Token(0)) {
		t.Error("SYSTEM SID must be privileged")
	}
	if isPrivilegedToken("S-1-5-21-1-2-3-1001", windows.GetCurrentProcessToken()) {
		t.Error("a non-SYSTEM SID with a primary token must read non-privileged")
	}
}
