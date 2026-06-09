# ADR-0002: x/crypto/ssh with explicit socket deadlines, never sshpass

**Status:** Accepted (2026-06-07)

## Context

sand shelled out to `sshpass`/`ssh(1)` with no ConnectTimeout and a bare await:
one black-holed probe (stale DHCP lease, mid-boot banner hang) froze the entire
daemon. ssh(1) has **no client option** that bounds a server that accepts TCP
but never sends its banner.

## Decision

All guest SSH goes through `golang.org/x/crypto/ssh` in-process, using this
recipe (spike-proven 2026-06-07 against live guests and synthetic failure
stand-ins):

```go
conn, err := net.DialTimeout("tcp", addr, timeout) // bounds TCP connect
_ = conn.SetDeadline(time.Now().Add(timeout))      // bounds banner + handshake + auth
sc, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
_ = conn.SetDeadline(time.Time{})                  // clear for session use
client := ssh.NewClient(sc, chans, reqs)
```

**Sharp edge this guards:** `ssh.ClientConfig.Timeout` covers TCP dial only —
the same blind spot as ssh(1)'s ConnectTimeout. Plain `ssh.Dial` silently
reintroduces sand's fatal flaw. The explicit socket deadline is the load-bearing
line; `internal/sshx` is the only place allowed to construct SSH clients.

Streaming exec uses `session.StdoutPipe()` + a reader goroutine, eliminating
the 64 KB pipe-buffer deadlock class.

## Spike evidence (2026-06-07)

| Scenario | sshpass/ssh(1) | x/crypto/ssh recipe |
|----------|----------------|---------------------|
| Healthy macOS guest | works | 296 ms to authed session, streaming output |
| Black-hole IP | hangs forever | fails at 3.002 s (3 s budget) |
| TCP accepted, banner never sent | hangs forever | fails at 3.003 s via socket deadline |

## Rejected alternatives

- **sshpass/ssh(1) subprocesses**: the incumbent; unboundable banner hang,
  process leaks, output parsing.
- **swift-nio-ssh**: see ADR-0001.
