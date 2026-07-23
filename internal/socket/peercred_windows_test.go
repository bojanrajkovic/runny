//go:build windows

package socket

import (
	"net"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// selfDialedConn builds a real AF_UNIX listen/dial/accept triangle within
// the test process and returns the accepted (server-side) conn — the same
// shape ServerHandshake sees, with this process as the connecting peer.
func selfDialedConn(t *testing.T) *net.UnixConn {
	t.Helper()
	sock := filepath.Join(shortSocketDir(t), "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	client, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	conn, err := ln.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn.(*net.UnixConn)
}

// ownTokenUser returns this test process's token-user SID string — what a
// correct peer read of a self-dialed connection must resolve.
func ownTokenUser(t *testing.T) string {
	t.Helper()
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatalf("GetTokenUser: %v", err)
	}
	return tu.User.Sid.String()
}

// TestProbePeerCred is the hardware validation of the whole arming story:
// the startup self-probe (throwaway socket, self-dial, getpeerpid ioctl
// with the trust-the-buffer workaround, PID and token-user SID round-trip)
// must succeed end to end against the kernel this test executes on. A
// failure here means the gate would (correctly, loudly) refuse to arm on
// this Windows build — which is exactly what a maintainer needs to know.
func TestProbePeerCred(t *testing.T) {
	if !probePeerCred() {
		t.Fatal("probePeerCred() = false: the getpeerpid ioctl chain does not work against this kernel")
	}
}

// TestReadPeerIDSelfDialed pins the production per-connection read over a
// real self-dialed AF_UNIX socket: the resolved identity must be this
// process's own token-user SID, and the privileged verdict must match an
// independent read of this process's own token (SYSTEM, or elevated
// Administrators membership via CheckTokenMembership on an identification-
// level duplicate) — computed, not hardcoded, since CI runners are
// typically elevated admins but developers need not be.
func TestReadPeerIDSelfDialed(t *testing.T) {
	conn := selfDialedConn(t)
	sc, err := conn.SyscallConn()
	if err != nil {
		t.Fatalf("SyscallConn: %v", err)
	}
	var (
		id         string
		privileged bool
		ok         bool
	)
	if err := sc.Control(func(fd uintptr) { id, privileged, ok = readPeerID(fd) }); err != nil {
		t.Fatalf("Control: %v", err)
	}
	if !ok {
		t.Fatal("readPeerID over a self-dialed socket reported unknown")
	}
	if want := ownTokenUser(t); id != want {
		t.Errorf("id = %q, want this process's own token-user SID %q", id, want)
	}

	// Independent expected-privileged computation from the current process
	// token, using the documented duplicate-then-CheckTokenMembership
	// sequence directly rather than the production helper. A real token
	// handle, not GetCurrentProcessToken's pseudo handle: duplication needs
	// TOKEN_DUPLICATE access, and the pseudo handle is query-only.
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatalf("CreateWellKnownSid: %v", err)
	}
	var tok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(),
		windows.TOKEN_QUERY|windows.TOKEN_DUPLICATE, &tok); err != nil {
		t.Fatalf("OpenProcessToken: %v", err)
	}
	defer tok.Close()
	var dup windows.Token
	if err := windows.DuplicateTokenEx(tok, windows.TOKEN_QUERY,
		nil, windows.SecurityIdentification, windows.TokenImpersonation, &dup); err != nil {
		t.Fatalf("DuplicateTokenEx: %v", err)
	}
	defer dup.Close()
	isAdmin, err := dup.IsMember(admins)
	if err != nil {
		t.Fatalf("IsMember: %v", err)
	}
	if want := id == systemSID || isAdmin; privileged != want {
		t.Errorf("privileged = %v, want %v (SID %s, elevated-admin %v)", privileged, want, id, isAdmin)
	}
}

// TestDecodePeerPID drives the pure trust-the-buffer decode with synthetic
// data: a sentinel-intact buffer (the kernel never wrote) and a written
// zero PID both fail; a written PID is read from the leading uint32
// regardless of what the trailing bytes hold.
func TestDecodePeerPID(t *testing.T) {
	for _, c := range []struct {
		name string
		buf  [8]byte
		pid  uint32
		ok   bool
	}{
		{"sentinel intact", peerPIDSentinel, 0, false},
		{"zero pid written", [8]byte{0, 0, 0, 0, 0xEE, 0xEE, 0xEE, 0xEE}, 0, false},
		{"pid written, trailing sentinel", [8]byte{0x39, 0x30, 0, 0, 0xEE, 0xEE, 0xEE, 0xEE}, 12345, true},
		{"pid written, fully overwritten", [8]byte{0x04, 0, 0, 0, 0, 0, 0, 0}, 4, true},
	} {
		pid, ok := decodePeerPID(c.buf)
		if pid != c.pid || ok != c.ok {
			t.Errorf("%s: decodePeerPID = (%d, %v), want (%d, %v)", c.name, pid, ok, c.pid, c.ok)
		}
	}
}
