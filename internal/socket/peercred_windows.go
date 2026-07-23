//go:build windows

package socket

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

// sioAFUnixGetPeerPID is the vendor-defined WSAIoctl control code
// (_WSAIOR(IOC_VENDOR, 256), implemented by afunix.sys) that returns the
// connecting peer's PID — the only peer-credential surface Windows AF_UNIX
// exposes; ancillary credential passing is deliberately unsupported there.
// The ioctl is undocumented by Microsoft, which is why the revocation gate
// arms only after probePeerCred proves it against the running kernel.
const sioAFUnixGetPeerPID = 0x58000100

// systemSID is the well-known SID of the SYSTEM account, this platform's
// unconditional privileged principal alongside elevated Administrators.
const systemSID = "S-1-5-18"

// peerPIDSentinel pre-fills the ioctl's output buffer. The ioctl has a
// long-standing upstream bug: it writes a valid PID into the buffer but
// reports zero bytes returned, so the byte count cannot distinguish success
// from a never-written buffer. A nonzero sentinel that survives the call
// untouched can: sentinel intact means the kernel never wrote, anything
// else is the written PID in the leading uint32. Verified still present on
// current Windows builds.
var peerPIDSentinel = [8]byte{0xEE, 0xEE, 0xEE, 0xEE, 0xEE, 0xEE, 0xEE, 0xEE}

// peerCredSupported is the per-platform arming check newOperatorGate runs
// when arming is requested: the probePeerCred self-probe, once per process.
// See probePeerCred for why an undocumented ioctl demands a live proof
// before the fail-closed gate may rely on it. A var so tests can pin both
// arming branches on any platform.
var peerCredSupported func() bool = sync.OnceValue(probePeerCred)

// readPeerID resolves the connecting peer's identity in two steps — the
// getpeerpid ioctl on the socket handle, then that process token's user
// SID — with the privileged verdict computed while the token is in hand.
// Both halves are shared verbatim with probePeerCred, so a probe pass
// proves this exact production path. Any failure reports unknown: the
// revocation gate fails closed on an unreadable identity by design, and the
// audit path records unknown rather than failing the request.
//
// The PID indirection is an accepted residual, unlike darwin's direct
// kernel credential: the peer could exit between the ioctl and OpenProcess,
// and its PID could in principle be reused. The read happens at handshake
// time, while the peer demonstrably just connected, narrowing that window
// to microseconds — and exploiting it requires write access to the socket,
// i.e. a principal already inside the authorization boundary.
func readPeerID(fd uintptr) (id string, privileged, ok bool) {
	pid, ok := peerPID(fd)
	if !ok {
		return "", false, false
	}
	return peerTokenIdentity(pid)
}

// peerPID issues the getpeerpid ioctl and decodes its output buffer.
func peerPID(fd uintptr) (uint32, bool) {
	buf := peerPIDSentinel
	var n uint32
	if err := windows.WSAIoctl(windows.Handle(fd), sioAFUnixGetPeerPID,
		nil, 0, &buf[0], uint32(len(buf)), &n, nil, 0); err != nil {
		return 0, false
	}
	return decodePeerPID(buf)
}

// decodePeerPID is the trust-the-buffer half of peerPID, pure so the
// sentinel-intact path is testable with synthetic data: a still-sentinel
// buffer (the kernel never wrote) or a written zero PID fail; otherwise the
// PID is the buffer's leading uint32.
func decodePeerPID(buf [8]byte) (uint32, bool) {
	if buf == peerPIDSentinel {
		return 0, false
	}
	pid := binary.LittleEndian.Uint32(buf[:4])
	return pid, pid != 0
}

// peerTokenIdentity opens pid's process token and reads its user SID plus
// the privileged verdict: SYSTEM, or an elevated member of Administrators —
// both hold Full ACEs on the daemon home from the install bootstrap's DACL
// conversion plus SeTakeOwnershipPrivilege, so a gate denial would be
// theater, the same rationale as root on darwin.
func peerTokenIdentity(pid uint32) (id string, privileged, ok bool) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "", false, false
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var tok windows.Token
	if err := windows.OpenProcessToken(h, windows.TOKEN_QUERY|windows.TOKEN_DUPLICATE, &tok); err != nil {
		return "", false, false
	}
	defer func() { _ = tok.Close() }()
	tu, err := tok.GetTokenUser()
	if err != nil {
		return "", false, false
	}
	id = tu.User.Sid.String()
	return id, id == systemSID || elevatedAdmin(tok), true
}

// elevatedAdmin reports whether tok belongs to an elevated member of
// Administrators. CheckTokenMembership answers from the token's own group
// list, so a UAC-filtered (non-elevated) admin — whose Administrators
// membership is deny-only in the filtered token — correctly reports false.
// The API requires an impersonation token, so the peer's primary token is
// duplicated at identification level first. Any failure reports false: the
// peer then simply gets no gate exemption and falls through to the ACL
// check — fail closed on the exemption, not on the whole identity.
func elevatedAdmin(tok windows.Token) bool {
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return false
	}
	var dup windows.Token
	if err := windows.DuplicateTokenEx(tok, windows.TOKEN_QUERY,
		nil, windows.SecurityIdentification, windows.TokenImpersonation, &dup); err != nil {
		return false
	}
	defer func() { _ = dup.Close() }()
	member, err := dup.IsMember(admins)
	return err == nil && member
}

// probePeerCred is the arming self-probe: listen on a throwaway AF_UNIX
// socket, dial it from this same process, and run the production read chain
// (peerPID + peerTokenIdentity — exactly what readPeerID composes) on the
// accepted connection, requiring this daemon's own PID and token-user SID
// back. Passing proves the undocumented ioctl still behaves on the running
// kernel; any failure leaves the gate unarmed with one loud error naming
// the failed step — feature loss with a loud log, falling back to the
// documented socket-is-the-sole-gate baseline, never a locked-out daemon
// and never a silent downgrade.
func probePeerCred() bool {
	fail := func(step string, err error) bool {
		slog.Error("windows peer-identity self-probe failed; the operator revocation gate stays unarmed and the socket ACL remains the sole gate",
			"step", step, "err", err)
		return false
	}

	// os.TempDir keeps the path under the sockaddr_un limit, which the
	// daemon home's deep ProgramData path is not guaranteed to.
	dir, err := os.MkdirTemp("", "runnyp")
	if err != nil {
		return fail("temp dir", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	sock := filepath.Join(dir, "p.sock")

	ln, err := net.Listen("unix", sock)
	if err != nil {
		return fail("listen", err)
	}
	defer func() { _ = ln.Close() }()
	// A self-dial completes against the listen backlog before Accept runs,
	// so neither call below should ever wait — the deadlines turn a
	// would-be wedge into a loud probe failure instead of a hung startup.
	_ = ln.(*net.UnixListener).SetDeadline(time.Now().Add(10 * time.Second))
	client, err := net.DialTimeout("unix", sock, 10*time.Second)
	if err != nil {
		return fail("dial", err)
	}
	defer func() { _ = client.Close() }()
	conn, err := ln.Accept()
	if err != nil {
		return fail("accept", err)
	}
	defer func() { _ = conn.Close() }()

	sc, err := conn.(*net.UnixConn).SyscallConn()
	if err != nil {
		return fail("syscall conn", err)
	}
	var (
		pid   uint32
		pidOK bool
	)
	if err := sc.Control(func(fd uintptr) { pid, pidOK = peerPID(fd) }); err != nil {
		return fail("fd control", err)
	}
	if !pidOK {
		return fail("getpeerpid ioctl", errors.New("no PID read (ioctl error, sentinel intact, or zero PID)"))
	}
	if pid != uint32(os.Getpid()) {
		return fail("peer PID", fmt.Errorf("ioctl resolved PID %d, want this daemon's own %d", pid, os.Getpid()))
	}
	id, _, ok := peerTokenIdentity(pid)
	if !ok {
		return fail("peer token", errors.New("opening the peer process token or reading its user SID failed"))
	}
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fail("own token user", err)
	}
	if own := tu.User.Sid.String(); id != own {
		return fail("peer SID", fmt.Errorf("resolved %s, want this daemon's own %s", id, own))
	}
	return true
}
