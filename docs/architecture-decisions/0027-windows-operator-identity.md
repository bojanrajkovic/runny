# ADR-0027: Windows operator identity — platform-native SID strings over the home DACL

**Status:** Accepted (2026-07-23)

## Context

The multi-operator surface ([ADR-0025](0025-per-rpc-operator-revocation.md)'s
grant/revoke/list RPCs, the per-RPC revocation gate, and the audit stamps on
injected keys and grant records) was built uid-first: `uint32` on the wire,
`uint32` on disk, `uint32` through the FSM. On Windows there is no numeric
account identity — `os/user.User.Uid` is a SID string
(`S-1-5-21-…`) — so the grant path's `strconv.ParseUint(u.Uid, …)` fails for
every Windows account, and nothing downstream of it could carry the identity
even if it parsed. Separately, the Windows daemon has no peer-credential read
wired up (`peercred_other.go`), so the revocation gate is unarmed there and
audit rows record the operator as unknown.

The install bootstrap already writes a home DACL on Windows
(`internal/sysdaemon`'s `icaclsHomeArgs`): an inheriting Modify grant for the
operator and the service SID, inheritance from ProgramData severed, the
BUILTIN\Users leak entry stripped. What was missing is the live half — the
same Grant/Revoke/List operations `internal/opacl` performs against darwin's
extended ACL, plus an identity representation those operations can traffic
in. All Windows ACL and socket mechanics below were validated on real
hardware (Windows 10 build 19045) against an installed system daemon.

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

**The Windows peer read stays on the existing AF_UNIX socket**, via the
`SIO_AF_UNIX_GETPEERPID` WSAIoctl resolved to the peer process token's SID
at handshake time. The ioctl's output buffer is trusted over its
bytes-returned value, working around the known bug where bytes-returned
reads zero despite a valid PID being written (microsoft/WSL#4676). Because
that ioctl is undocumented surface, the revocation gate arms only after a
startup self-probe: the daemon dials its own socket and requires its own PID
and SID back. Probe failure logs loudly and leaves the gate unarmed — the
socket-is-the-sole-gate baseline `docs/security.md` documents as the primary
tier, never a silently weaker gate. (The peer-read half ships in a follow-up
PR; this ADR records the whole decision.)

**opacl gains a Windows implementation over the same home DACL the install
bootstrap writes.** Grant/Revoke run `icacls` (the home dir gets the
bootstrap's exact `(OI)(CI)M` ACE shape, so bootstrap and live-granted
operators are indistinguishable to List; the live socket gets a
belt-and-braces explicit stamp). ListIDs reads the DACL via
`GetNamedSecurityInfo`/`GetAce`; membership is an ACCESS_ALLOWED ACE, not
INHERIT_ONLY, mask carrying `FILE_WRITE_DATA`, SID resolving to
`SidTypeUser` — which selects exactly the operator accounts and excludes the
service SID, SYSTEM, Administrators, and CREATOR OWNER that the bootstrap
also writes. SYSTEM and elevated Administrators bypass the gate entirely
(they hold Full ACEs from the bootstrap's DACL conversion, plus
`SeTakeOwnershipPrivilege`) — darwin's uid-0 rationale — and are refused as
grant targets for the same reason root is: an ACE for an account the ACL
cannot actually constrain would be a lie in the operator list.

## Rejected alternatives

- **A unified cross-platform identity type** (a tagged uid-or-SID struct on
  wire and disk). Churns every darwin record, message, and reader to buy
  comparability that nothing needs — no code path ever compares a darwin
  operator to a Windows one. The opaque string plus additive sid fields
  keeps darwin's shipped shape byte-stable.

- **Mapping SIDs to synthetic uint32 ids** to keep the existing wire type.
  RIDs collide across machines, virtual service accounts have no meaningful
  numeric identity at all, and the mapping table is new persistent state
  invented to serve a wire type the platform has outgrown.

- **A named-pipe control channel with `ImpersonateNamedPipeClient`.**
  Token-attested peer identity is genuinely stronger than PID indirection,
  but a pipe's security descriptor is fixed per-listener at creation and has
  no filesystem path — it splits the one-ACL model, so live grant/revoke
  would need a second ACL mechanism kept in sync with the home DACL, and the
  control transport forks per platform. Reopen if the self-probe fails on a
  current supported Windows build, or if PID indirection is shown
  exploitable beyond the accepted operator-only residual.

- **Leaving the gate permanently unarmed on Windows.** Zero new surface, but
  it leaves the lingering-connection revocation gap and unattributed audit
  records on exactly one platform, for the cost of a handshake-time read
  plus a startup probe.

## Consequences

- Windows reaches operator parity: grant/revoke/list against the home DACL,
  SID-string identities in grant records, injected-key audit rows, and
  `runnyctl` output (rendered as the resolved `DOMAIN\name` plus the SID).
- The documented residual: Windows peer identity is PID-indirected (the
  ioctl attests the peer PID; the SID comes from opening that process's
  token), not token-attested like darwin's `LOCAL_PEERCRED`. Exploiting the
  indirection requires a principal already inside the authorization
  boundary, in an audit trail documented as good-faith rather than
  tamper-proof.
- `peerCredSupported` becomes a per-platform arming check backed by the
  self-probe, so a future Windows regression in the undocumented ioctl
  degrades loudly to the documented socket-only baseline instead of denying
  every RPC or silently mis-attributing.
- Reinstall preserves live grants on Windows: the install ACL sequence
  deliberately converts and keeps existing explicit entries rather than
  resetting them (the `/inheritance:d` trade-off recorded in
  `internal/sysdaemon`), unlike darwin's reset-to-bootstrap. A revoked-then-
  reinstalled host therefore keeps its live-granted operators; revocation,
  not reinstall, is the removal path.
