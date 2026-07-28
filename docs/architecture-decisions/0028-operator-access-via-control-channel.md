# ADR-0028: The operator ACL is an identity registry; operator access to daemon state is the control channel

**Status:** Accepted (2026-07-28)

## Context

The system daemon's home carried a **dual inheriting ACL**: an entry for the
service account so the daemon could read operator-landed files, and an entry for
each operator granting directory write. Both inherited, so every artifact the
daemon ever wrote — `images/`, `vms/`, `cycles/`, `logs/` — received a private
copy of every operator's entry at creation.

An inherited copy cannot be reached from the directory it came from. Removing
the operator's entry from the home therefore removed exactly one entry and left
the copies untouched, so a revoked operator kept write access to everything the
daemon had produced up to that moment. Fixing *that* is what the entry's
inheritance had made expensive: the grant and revoke paths grew a second target,
then a tree walk, then per-platform normalization, and three review rounds
produced eleven findings against two attempts. Every one of them was about
propagating or un-propagating an entry across a tree. None asked whether the
tree needed the entry.

It does not. An audit of every path that touches the daemon's home from a client
found a single reader — `edit-config` seeding its editor — plus `upgrade-daemon`
validating the live config by handing its path to a `runnyd -test-config`
subprocess running as the operator. Logs, status, cycle history, prune and key
injection were already RPCs. Two callers, both reading one file.

Four platform behaviours were measured against real hosts, because the two
platforms had needed *opposite* fixes and the reason was not in any doc:

- **Windows re-propagates an inheritable entry to children that already exist**
  when the parent's ACL changes; a child created before a non-recursive grant
  showed the entry marked inherited. **darwin does not** — inheritance there is
  copy-at-create, and the equivalent child had no ACL at all. The
  "no recursive re-stamp" rationale that the code cited was a darwin property
  that had been carried to Windows, where it is false.
- `icacls <child> /reset` strips an explicit duplicate and leaves the inherited
  entry; darwin's `chmod -N` leaves the child bare.
- A recursive ACL walk over 876 objects takes ~200ms, so the operation bound was
  never the constraint the design had treated it as.
- The renamed-in file carries **the renamer's** ownership and umask on both
  platforms, so any design where an operator renames a file into the daemon's
  home hands the daemon a config it does not own.

## Decision

**The operator entry sits on the home directory and does not inherit.** It has
exactly two jobs, both of which stop at the directory: it is the **operator
registry** — the membership read that answers "is this caller an operator", for
the per-RPC revocation gate ([ADR-0025](0025-per-rpc-operator-revocation.md)) —
and it is the directory access needed to reach the control socket on darwin and
to land an App key by hand. There is nothing below the directory to propagate,
nothing to normalize, and no partial walk to detect. Grant and revoke are one
command against one object on both platforms.

**Everything else an operator reads or writes in the home goes over the control
channel.** Reading and replacing `config.yaml` become RPCs carrying **raw
bytes** — not a parsed document, because a human reads and re-edits what comes
back, so comments, key order, quoting and the schema modeline have to survive
the round trip (the install-time staging path already holds this line by
substituting bytes rather than re-marshalling). `edit-config` seeds from the
read RPC and applies through the write RPC; `upgrade-daemon` validates the bytes
the read RPC returns, since its gate must run the *new* binary and so cannot be
delegated to the running daemon.

**The daemon performs the write.** This is the decision's load-bearing half, not
an implementation detail: an operator cannot chown a file to the service account
— only root can — and neither can the non-root daemon. Routing the write through
the owner is the only mechanism that keeps `config.yaml` owned by the daemon
regardless of which operator authored the edit. A client-side rename is a
one-way door on ownership.

**The write RPC does not validate.** The running binary's parser is the wrong
authority: `upgrade-daemon` exists to move a config forward past what the
running binary accepts, which is why its reload verb can defer a parse failure
to the respawn target. A parser gate on the write would re-break that.
Validation stays where it already is — client-side against the binary being
upgraded to, and in the reload path's exit gate, which holds the exit rather
than respawning onto a config that will not load.

**A missing config and an unreadable one are distinct answers**, all the way to
the client. `edit-config` seeds a blank skeleton when no config exists, and that
is destructive if a read that merely *failed* can reach the same branch.

## Rejected alternatives

- **Keep the inheriting entry and manage the propagation.** The status quo, and
  what two prior attempts built — a second grant target, then a recursive
  re-stamp on grant and revoke, then per-platform normalization for the
  asymmetry above. It is a larger, more platform-specific mechanism whose
  correctness rests on a tree walk completing; a walk killed mid-flight leaves
  a partially-stamped tree, and on darwin `chmod -R` is pre-order, so the home
  directory — the obvious thing to check afterwards — is stamped *first* and
  therefore cannot witness the failure. Eleven review findings landed on this
  shape without exhausting it.

- **Recursively re-stamp on grant and revoke only** (no normalization). Smaller,
  but it converts every grant and revoke into an operation whose blast radius is
  the whole home and whose failure mode is a silently half-updated tree — in a
  system whose central invariant is that failures are loud. It also leaves the
  two platforms diverging, since Windows re-propagates on its own and darwin
  does not.

- **A group as the ACL principal, with grant/revoke as group membership
  changes.** One entry on the tree forever, membership changes taking effect
  without touching a single ACL — genuinely the cleanest model for the *access*
  question. Rejected on size: it moves operator identity from a per-account ACE
  the daemon reads directly to a platform group whose membership must be created
  at install, synchronized on every host, and read back through a directory
  service that can be a domain controller on Windows — the exact per-RPC lookup
  latency the revocation gate is built to avoid.

- **Making App-key landing an RPC too**, leaving the home entry a pure identity
  marker with no filesystem role. It is the logical end of this direction, and
  it is rejected here because it puts private key material on the control
  channel to remove a capability that costs nothing to keep. The hand-landed
  key stays a documented filesystem flow.

## Consequences

- **Revocation means what it says.** Removing an operator removes their access,
  rather than their access to one directory plus a permanent private grant on
  every artifact created before the revoke.

- **The unverified assumption stops being load-bearing.** Whether a Windows
  non-administrator can rename a file over one it has no access to — documented
  to hold, but never successfully measured here across two attempts — was
  load-bearing while the operator applied edits by renaming. With the daemon
  performing the write, no shipped path depends on it.

- **A stopped system daemon cannot be config-edited by an operator.** The read
  falls back to a direct one, which succeeds only for the file's owner; the
  recovery path is `sudo runnyctl install-daemon --config`, which re-stages
  idempotently. This is a real capability loss on Windows, where the previously
  inherited entry made the file directly editable, and it is the price of the
  revoke above. The failure is loud and names both attempts.

- **The two platforms converge.** Grant and revoke are one command against one
  object on both; the re-propagation asymmetry stops mattering because nothing
  below the directory carries the entry.

- **The darwin control socket is stamped from the operator set at creation.** It
  lives inside the home and had been relying on inheritance; with a
  non-inheriting entry it would otherwise carry none, locking every operator out
  of the daemon on the next restart. Windows already worked this way — its pipe
  security descriptor is built when the pipe is created and inherits nothing.

- **This amends the access-boundary halves of
  [ADR-0020](0020-headless-system-daemon.md) and
  [ADR-0027](0027-windows-operator-identity.md).** The service account's entry
  still inherits, and still recursively, and that asymmetry is deliberate: it is
  installed once and never revoked, and the daemon does need to reach what it
  writes — including a `0600` file an operator landed and owns.
