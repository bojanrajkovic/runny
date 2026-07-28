# ADR-0027: Windows operator identity — platform-native SID strings, read by named-pipe impersonation

**Status:** Accepted (2026-07-23)

**Amended:** 2026-07-28 — the home ACE's *shape* is superseded in part by
[ADR-0028](0028-operator-access-via-control-channel.md): the operator entry no
longer inherits, on either platform, so the bootstrap's `(OI)(CI)M` below is now
a plain `M` on the home directory alone. The identity-string, impersonation
peer-read, membership-rule and grant-target-exclusion halves are unchanged — the
DACL is still where operator membership is read from, and `FILE_WRITE_DATA` is
still the bit that defines it.

**Amended:** 2026-07-23 — the peer-read half of this decision is reversed. The
original plan kept the Windows peer read on the existing AF_UNIX socket via the
`SIO_AF_UNIX_GETPEERPID` ioctl resolved through `OpenProcess` to the peer's
token SID, arming the gate only behind a startup self-probe; the named-pipe
`ImpersonateNamedPipeClient` transport was recorded here as a *rejected*
alternative. Real hardware reversed that: cross-principal `OpenProcess` from the
unprivileged `NT SERVICE\runnyd` account is target-dependent — it opened an
admin user's token but was **denied** a `LOCAL SERVICE` process, with no
`SeDebugPrivilege` to force it — and the arming self-probe can only ever open
the daemon's *own* process, so it cannot detect that failure. The named-pipe
impersonation read was then spike-validated on the real service account reading
every principal, including the one `OpenProcess` refused. The identity-string
and opacl-over-the-home-DACL halves are unchanged; the Decision, Rejected
alternatives, and Consequences below are updated in place rather than left
describing a peer read the shipped code never used.

## Context

The multi-operator surface ([ADR-0025](0025-per-rpc-operator-revocation.md)'s
grant/revoke/list RPCs, the per-RPC revocation gate, and the audit stamps on
injected keys and grant records) was built uid-first: `uint32` on the wire,
`uint32` on disk, `uint32` through the FSM. On Windows there is no numeric
account identity — `os/user.User.Uid` is a SID string
(`S-1-5-21-…`) — so the grant path's `strconv.ParseUint(u.Uid, …)` fails for
every Windows account, and nothing downstream of it could carry the identity
even if it parsed. Separately, the Windows daemon had no peer-credential read
wired up, so the revocation gate was unarmed there and audit rows recorded the
operator as unknown.

The install bootstrap already writes a home DACL on Windows
(`internal/sysdaemon`'s `icaclsHomeArgs`): an inheriting Modify grant for the
operator and the service SID, inheritance from ProgramData severed, the
BUILTIN\Users leak entry stripped. What was missing is the live half — the
same Grant/Revoke/List operations `internal/opacl` performs against darwin's
extended ACL, plus an identity representation those operations can traffic in,
plus a peer read the gate can enforce against. All Windows ACL, pipe, and
impersonation mechanics below were validated on real hardware against an
installed system daemon.

## Decision

**Operator identity is an opaque platform-native string, exactly
`os/user.User.Uid`'s convention: a decimal uid on darwin, a SID string on
Windows.** On the wire and on disk the string is additive: `InjectedKey`,
`OperatorMutation`, `Operator`, and the `operator-grants.jsonl` records gain
`sid` fields beside the existing uint32 uid fields; one shared helper stamps
numeric identities into the legacy uid field and everything else into the
sid field, so darwin records are byte-for-byte unchanged and no stamp site
switches on platform. No unified cross-platform identity type exists because
nothing needs cross-platform identity comparison — every ACL and every audit
trail is host-local.

**The Windows control channel is a named pipe (`\\.\pipe\runnyd`), and the peer
identity is read by impersonating the connecting client at the handshake.**
`ImpersonateNamedPipeClient` attaches the client's kernel-established security
context to the reading thread; `OpenThreadToken` + `GetTokenUser` then recover
the client SID with **no process open at all**, sidestepping the cross-principal
`OpenProcess` denial entirely. The read never fails on a live connection, so the
gate arms unconditionally on the system daemon — no startup self-probe, no
unarmed fallback, no `peerCredSupported=false` degradation. The client dials at
`SECURITY_IDENTIFICATION`, which lets the daemon read identity and group
membership but grants it no right to *act as* the client (winio's default
Anonymous level yields an unreadable token). The transport forks per platform
behind a small seam (`listen`/`dial`/`SocketPath`, `readPeer`): darwin keeps the
unix socket + `SO_PEERCRED`; the shared gRPC server, gate, and RPC handlers are
untouched.

**opacl gains a Windows implementation over the same home DACL the install
bootstrap writes.** Grant/Revoke run `icacls` against the home dir only — the
pipe has no filesystem object to stamp, so unlike darwin there is no second
live-socket target. The home ACE is the bootstrap's exact `(OI)(CI)M` shape, so
bootstrap and live-granted operators are indistinguishable to List. ListIDs
reads the DACL via `GetNamedSecurityInfo`/`GetAce`; membership is an
ACCESS_ALLOWED ACE, not INHERIT_ONLY, whose mask carries `FILE_WRITE_DATA`, for
a SID that is not a well-known or service principal. That last exclusion is
**structural, by SID-string prefix** — `S-1-5-18/19/20`, the `S-1-5-32-`
aliases, the `S-1-5-80-` service range — applied *before* trusting
`LookupAccountSid`'s type, because in the daemon's own process context
`LookupAccountSid` reports the service SID `NT SERVICE\runnyd` as `SidTypeUser`
and would otherwise count the daemon itself as an operator. SYSTEM and an
elevated Administrators member bypass the gate (they own the machine and the
home DACL and hold no operator ACE, read from the same impersonation token —
SYSTEM by SID, elevated admin by `CheckTokenMembership`; a UAC-filtered
non-elevated admin reads false) and are refused as grant targets, the same
already-privileged rationale as darwin's root.

**The pipe's security descriptor is a coarse connect filter, not the outer
authorization tier.** It grants connect to Authenticated Users only; the per-RPC
revocation gate is the primary Windows tier. This is a deliberate tier shift
from darwin, where the socket's own `0600` + inherited ACL is the outer gate: on
Windows an authenticated user who holds no operator ACE can *open* the pipe, but
every RPC fails closed and loud (`PermissionDenied`), and the same live revoke +
in-flight stream kill apply.

## Rejected alternatives

- **A unified cross-platform identity type** (a tagged uid-or-SID struct on
  wire and disk). Churns every darwin record, message, and reader to buy
  comparability that nothing needs — no code path ever compares a darwin
  operator to a Windows one. The opaque string plus additive sid fields keeps
  darwin's shipped shape byte-stable.

- **Mapping SIDs to synthetic uint32 ids** to keep the existing wire type.
  RIDs collide across machines, virtual service accounts have no meaningful
  numeric identity at all, and the mapping table is new persistent state
  invented to serve a wire type the platform has outgrown.

- **The AF_UNIX socket + `SIO_AF_UNIX_GETPEERPID` ioctl + `OpenProcess`
  peer read.** Keeps a single transport, but the SID comes from *opening the
  peer's process*, and cross-principal `OpenProcess` from the unprivileged
  service account is target-dependent — hardware-confirmed to open an admin
  user's token yet be denied a `LOCAL SERVICE` process, with no
  `SeDebugPrivilege` available to force it. Worse, the startup self-probe meant
  to gate arming can only open the daemon's own process, so it is structurally
  blind to exactly the cross-principal denial that breaks the real read. PID
  indirection is also weaker than a token-attested identity. The pipe
  impersonation read has none of these: it reads the client SID off the
  kernel-established connection with no process open.

- **Granting the daemon `SeDebugPrivilege`** to force the `OpenProcess` read.
  Machine-wide god-mode (open any process on the host) traded for one narrow
  identity read — the opposite of this project's least-privilege service
  account, and a far larger blast radius than the problem.

- **Leaving the gate permanently unarmed on Windows.** Zero new surface, but it
  forgoes live revoke and operator attribution on exactly one platform — the
  lingering-connection gap ADR-0025 closed everywhere else — for the cost of a
  handshake-time impersonation read that turned out to have no failure mode to
  degrade around.

## Consequences

- Windows reaches operator parity: grant/revoke/list against the home DACL,
  SID-string identities in grant records, injected-key audit rows, and
  `runnyctl` output; and a live, token-attested revocation gate — no PID
  indirection, parity with darwin's `LOCAL_PEERCRED` rather than a documented
  weaker residual.
- The control transport forks per platform behind a small seam (`listen`,
  `dial`, `home.SocketPath`, `readPeer`); the gRPC server, the revocation gate,
  and every RPC handler are platform-agnostic above it. The pipe conn carries
  the raw HANDLE (winio promotes `Fd()`) so the one impersonation at handshake
  can reach it; the handshake peeks one byte first (a client message must be
  pending to impersonate against) and replays it so the HTTP/2 stream loses
  nothing.
- Windows authorization is a two-tier model that differs from darwin's *for the
  system daemon*: the pipe SD is a coarse connect filter and the per-RPC gate is
  primary, rather than the socket's own mode being the outer gate. A *per-user*
  Windows daemon keeps darwin's single-owner shape — its pipe name is derived
  from the resolved home (distinct per user, but derivable: an unsalted hash of
  a guessable path, so the client owner-verifies it like the system pipe, by
  identity rather than privilege) and its SD grants
  connect to the owning user's SID alone, the owner-only analogue of the 0600
  per-user socket; the gate stays unarmed there, as on a per-user home
  everywhere (a single owner, no ACL-managed operator set). Documented in
  `docs/security.md`.
- `peerCredSupported` is unconditionally true on Windows — the gate always arms
  on the system daemon, with no probe to fail — where the darwin value tracks
  the real `SO_PEERCRED` capability.
- Reinstall preserves live grants on Windows: the install ACL sequence
  deliberately converts and keeps existing explicit entries rather than
  resetting them (the `/inheritance:d` trade-off recorded in
  `internal/sysdaemon`), unlike darwin's reset-to-bootstrap. A revoked-then-
  reinstalled host therefore keeps its live-granted operators; revocation,
  not reinstall, is the removal path.
