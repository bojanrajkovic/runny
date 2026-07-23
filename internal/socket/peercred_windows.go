//go:build windows

package socket

import (
	"net"
	"runtime"

	"golang.org/x/sys/windows"
)

// peerCredSupported is true on windows: the named-pipe control channel lets the
// daemon read the connecting client's SID by impersonating the connection at
// the handshake — a kernel-established identity, not a client-supplied value
// and not derived from opening the client process (which the unprivileged
// service account cannot do across principals). The gate arms unconditionally
// on the system daemon; there is no unreadable-identity failure mode to degrade
// around, so no startup self-probe.
const peerCredSupported = true

// ImpersonateNamedPipeClient is not in x/sys/windows or go-winio; bind it off
// advapi32 directly. It attaches the connecting client's security context to
// the calling thread until RevertToSelf.
var (
	advapi32                       = windows.NewLazySystemDLL("advapi32.dll")
	procImpersonateNamedPipeClient = advapi32.NewProc("ImpersonateNamedPipeClient")
)

// systemSID is NT AUTHORITY\SYSTEM. A pipe client running as SYSTEM is a
// privileged principal (it owns the machine); matched by SID string so the
// verdict needs no token-membership call.
const systemSID = "S-1-5-18"

// readPeer reads the connecting pipe client's SID by impersonation. winio's
// pipe conn promotes Fd() to the raw HANDLE; without it (a foreign conn, or a
// test bufconn) the identity is unknown and the gate fails closed.
//
// ImpersonateNamedPipeClient needs a client message pending in the pipe, so we
// peek one byte first. gRPC's client writes its HTTP/2 connection preface the
// instant the transport connects, so the read returns promptly — and it runs
// under the deadline gRPC stamps on the conn around ServerHandshake, so a
// client that connects but never writes is timed out, not hung on forever. The
// peeked byte is handed back via a replay wrapper so the HTTP/2 stream loses
// nothing.
func readPeer(conn net.Conn) (net.Conn, string, bool, bool) {
	fdConn, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		return conn, "", false, false
	}
	handle := windows.Handle(fdConn.Fd())

	var b [1]byte
	n, err := conn.Read(b[:])
	if n == 1 {
		conn = &replayConn{Conn: conn, prefix: b[:1]}
	}
	if err != nil || n != 1 {
		return conn, "", false, false
	}

	id, privileged, ok := impersonateAndRead(handle)
	return conn, id, privileged, ok
}

// impersonateAndRead impersonates the pipe client on a locked OS thread, reads
// the thread token's user SID, decides the privileged verdict from that same
// token, then reverts. LockOSThread pins the impersonation to this goroutine's
// thread so no other work runs under the client's context. Any failure returns
// unknown (ok=false) → the gate fails closed, the audit records "unknown",
// exactly like darwin's cred-read miss.
func impersonateAndRead(handle windows.Handle) (id string, privileged, ok bool) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if r, _, _ := procImpersonateNamedPipeClient.Call(uintptr(handle)); r == 0 {
		return "", false, false
	}
	defer windows.RevertToSelf()

	var tok windows.Token
	if err := windows.OpenThreadToken(windows.CurrentThread(), windows.TOKEN_QUERY, false, &tok); err != nil {
		return "", false, false
	}
	defer tok.Close()

	tu, err := tok.GetTokenUser()
	if err != nil {
		return "", false, false
	}
	sid := tu.User.Sid.String()
	return sid, isPrivilegedToken(sid, tok), true
}

// isPrivilegedToken reports whether the impersonated client is an
// always-authorized principal: SYSTEM (by SID), or an elevated member of the
// built-in Administrators alias. IsMember wraps CheckTokenMembership, which
// accepts an identification-level impersonation token (the level the client
// dials at) and returns false for a UAC-filtered non-elevated admin — the
// Administrators group is deny-only in a filtered token — so an un-elevated
// admin correctly reads non-privileged. On any error it errs to
// non-privileged: the safe direction is to require an explicit operator ACE.
func isPrivilegedToken(sid string, tok windows.Token) bool {
	if sid == systemSID {
		return true
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return false
	}
	member, err := tok.IsMember(admins)
	return err == nil && member
}

// replayConn re-serves bytes peeked off the pipe during the handshake before
// delegating to the underlying conn, so the impersonation read costs the HTTP/2
// stream nothing.
type replayConn struct {
	net.Conn
	prefix []byte
}

func (c *replayConn) Read(b []byte) (int, error) {
	if len(c.prefix) > 0 {
		n := copy(b, c.prefix)
		c.prefix = c.prefix[n:]
		return n, nil
	}
	return c.Conn.Read(b)
}
