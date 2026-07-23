# Security

runny's security posture, stated as current facts. This is the canonical home
for security topics: where a control is decided in an ADR or maintained by a
contributor rule, this doc states what the control *guarantees* and links to
the decision or rule behind it. Other docs point here rather than restating.

## Supply chain and CI

- **Workflows are audited by [zizmor](https://github.com/zizmorcore/zizmor)** on
  every commit (pre-commit) and in CI; findings fail the build. The workflows
  are themselves attack surface — template injection, unpinned actions,
  over-broad permissions, leaked tokens — and zizmor is the gate over them.
- **Every action is pinned by commit SHA**, never a movable tag; Renovate keeps
  the pins current.
- **Release artifacts build cold** — no Bazel or tool cache feeds the artifact
  job, so a poisoned cache cannot reach a deployable binary — and every build
  carries a **provenance attestation**. The Homebrew cask (`runny-app`) is
  rendered in the same release job from `tools/deploy/runny-app.rb.tmpl`,
  pointing at the same attested, notarized `.dmg`; no new signing posture.
- **Checkouts never persist credentials**; workflow permissions are
  least-privilege, declared per job, and GitHub App tokens declare explicit
  `permission-*` scopes rather than inheriting the installation's full rights.
- **Release credentials are environment-scoped, one environment per trust
  domain**: the `releaser-app` environment holds the GitHub App key (jobs that
  mint App tokens; `main` and `v*` refs only), and the `release` environment
  holds signing/notary credentials (the artifact-signing job; `v*` tag refs
  only). A job declares at most one — token-minting jobs can never read
  signing keys. Provisioning is `tools/setup-secrets.sh`.
- **`main` is protected**: pull-request-only, required status checks, signed
  commits, linear history, no force-push or deletion.
- **Secret scanning and push protection** are enabled on the repository.

The rules a contributor follows to keep this intact live in
[CONTRIBUTING.md](../CONTRIBUTING.md) ("CI security"); release mechanics are
[ADR-0010](architecture-decisions/0010-release-engineering.md).

## Guest network isolation

**Concurrent guests cannot reach each other over the network**, by default and
not configurably.

runny attaches every guest with `VZNATNetworkDeviceAttachment`
(`internal/vm/vz_darwin.go`), which enables vmnet's bridge isolation. Guests
share the `192.168.64.0/24` NAT subnet, but the bridge drops guest-to-guest
frames while still allowing each guest to reach the gateway and the host to
reach each guest. A compromised job cannot pivot to a sibling guest's services
(e.g. `ssh`) — the packets have no route. Apple exposes no API to disable, or
further tighten, this; it is a fixed property of the attachment.

**Residual risk.** Bridge isolation does not defend against ARP spoofing (a
guest poisoning the bridge's ARP table to intercept *host↔guest* traffic) or
DHCP-pool exhaustion under heavy guest churn. The userspace packet filter
[softnet](https://github.com/openai/softnet) closes both, but runs as a
SUID-root helper holding the `com.apple.vm.networking` entitlement — a
privileged runtime dependency against the no-runtime-binary posture
([ADR-0008](architecture-decisions/0008-native-virtualization-framework.md)).
It is not adopted: the threats it defends require an untrusted multi-tenant
fleet, out of scope at current scale. Adopting it would be ADR-worthy.

The SSH host-key pinning below narrows what the ARP-spoof residual buys an
attacker: redirecting the host's SSH traffic to a fake guest fails the pin
check instead of harvesting credentials. What remains is the
trust-on-first-use window — an *active* MITM already present at a cycle's
first connection can proxy it — conceded with the same at-current-scale
rationale as softnet ([ADR-0013](architecture-decisions/0013-ephemeral-ssh-keys-in-band-rotation.md)).

## Guest access

Guest control is **SSH only**, through `x/crypto/ssh` with mandatory socket
deadlines — never an external `ssh`/`sshpass` binary
([ADR-0002](architecture-decisions/0002-x-crypto-ssh-with-socket-deadlines.md)).

**The image's baked password is a bootstrap credential, used once per cycle.**
By default (`ssh_hardening: rotate`,
[ADR-0013](architecture-decisions/0013-ephemeral-ssh-keys-in-band-rotation.md)),
the first authenticated session of each cycle mints an in-memory ed25519
keypair, installs the public key, **disables password authentication** in the
guest, and reconnects authenticated by the key with the guest's **host keys
pinned**. The private key never touches disk and dies with the daemon; the
guest cannot outlive it (in-process VMs,
[ADR-0008](architecture-decisions/0008-native-virtualization-framework.md)).
The GitHub JIT config is minted strictly *after* the rotation, so it only
ever crosses the keyed, pinned channel. Auth selection is exclusive — a guest
that rejects the cycle key fails the cycle loudly rather than silently
falling back to the password.

Pools can opt out per pool (`ssh_hardening: off`), which preserves
password-auth-for-the-whole-cycle behavior; the network posture above is then
what bounds the password (reachable from the host datapath only, not from
sibling guests).

Pools can also opt further **in** (`ssh_hardening: scramble`): the same
pre-flip exec that installs the cycle key and disables password auth also
randomizes the guest account's password. The image's well-known default is
never reachable again for the rest of the cycle, through any channel — not
just SSH password auth. The new value is generated in memory, installed
once, and discarded; the daemon authenticates by key from here on and never
needs to recover it. Opt-in by design: some workflows rely on the known
guest password (interactive console access, password-auth tooling), so this
has to be a deliberate choice, not the default.

## Operator debug keys

`runnyctl debug` injects an operator's SSH public key into a live guest's
`authorized_keys` for incident response
([ADR-0014](architecture-decisions/0014-debug-key-injection.md)). The
credential is **per-guest, per-cycle**: it is appended alongside the cycle key
(no sshd config change, works under `ssh_hardening: off`) and dies with the
clone at teardown — there is nothing to revoke. On the idle path the runner is
**verifiably killed** (a pgrep read-back proving the listener dead) *before*
the key lands, so a frozen guest cannot pick up a job with an operator
credential present.

A job **may run with an operator credential present**, by explicit consented
operator action (the mid-job inject path) — runny's contract here is
**visibility, not prevention**. Every such event is recorded three ways: the
job record itself names the fingerprints (`JobInfo.operator_keys`); every
attempt — including failed and refused ones — is in `CycleRecord.injected_keys`
with its FSM state and outcome; and a write-ahead `operator-access.json`
sidecar is written *before any byte reaches the guest*, survives a daemon
crash, and is surfaced in `runnyctl why` for the cycle's **retention window**
(not indefinitely — retention still ages it).

**Session recording is best-effort reconstruction, not enforcement.** Each
operator key is installed with a `command=` wrapper that appends every session
it authorizes — including direct reconnects to the guest — to a
`debug-session.log` artifact pulled at teardown. This is an audit/reconstruction
aid for good-faith operators, **not a security control.** The operator owns
`~/.ssh/authorized_keys` on the guest and can strip the wrapper; under the
default image no file-ownership scheme changes this — the guest ships a
well-known default password and `admin` holds sudo, so the operator who can run
`runnyctl debug` can already reach root. `ssh_hardening: scramble` does not
close this on its own: the operator's own debug-key session already has
write access to `~/.ssh/authorized_keys` and passwordless sudo, neither of
which needs the account password. Enforced capture needs a different guest
shape entirely: a **custom guest image** with `ForceCommand`/auditd baked
into `sshd_config` at image-build time — root-owned, so the operator cannot
edit it — paired with a separate, non-`admin` operator account so the
operator's own sudo doesn't reach that root-owned config either. `scramble`
(above) is what keeps that combination airtight once built: with the
operator no longer owning the config or holding sudo, a still-known default
password would be the one remaining door back into `admin`.

Authorization is the socket itself: the `0600` `runnyd.sock` is the sole gate,
deliberately — whoever can open it already transitively holds everything
injection grants (the config that can set `ssh_hardening: off`, the App key, the
daemon binary). For a per-user agent the socket is owner-only. For the headless
system daemon (below) it is owned by the `_runny` service account and reachable
by the operator account through the home's inheriting ACL — intentional: that
operator is the trusted administrator who landed the App key and edits the
config, so it already holds that same transitive power. The audit trail is the
accountability layer, not a second authorization tier.

**Each `injected_keys` entry is stamped with the authenticated peer.** The
daemon reads the connecting operator's identity server-side on the `0600`
socket (`SO_PEERCRED`'s uid on darwin; on Windows the AF_UNIX getpeerpid
ioctl resolved to the connecting process token's SID) — authoritative and
not client-forgeable — and records it alongside a best-effort username
snapshot, surfaced in `runnyctl why` (e.g. "by bob (uid 503)"). A root
peer, which bypasses the socket's `0600` mode, is recorded as `uid 0`; an
identity the daemon cannot read (a platform with no peer read, or a
cred-read failure) is recorded as unknown rather than failing the request —
an audit enhancement, never a second gate. This closes the "an operator did
X" → "operator A did X" gap once more than one operator identity can exist;
see [ADR-0014](architecture-decisions/0014-debug-key-injection.md) for the
authorization model this extends. The routine lifecycle commands — slot and
fleet mutations that grant no credential — log the same kernel-authenticated
uid and best-effort username to the daemon log (`runnyctl logs --daemon`):
attribution only, no new persisted audit trail.

**The control socket may grant several operator accounts, on the system
daemon.** An existing operator grants another via `runnyctl operator grant`:
the daemon, which owns its home, adds an inheriting ACL entry for the new
account to the home dir (future artifacts) and directly to the live socket
(no restart) — reaching the `GrantOperator`/`RevokeOperator` RPCs at all
already means the caller is an operator, so granting another is transitive
trust, not a new gate, the same posture ADR-0014 already established for
debug-key injection. Grants are attributed in an `operator-grants.jsonl`
audit trail under the operator-writable home — a good-faith reconstruction
aid, not a tamper-proof control, exactly like the `injected_keys` trail and
the guest `authorized_keys` before it. `runnyctl operator revoke` refuses to
remove the last operator (recoverable only via `sudo runnyctl
install-daemon`, which resets the ACL to the install-time bootstrap). A root
peer is refused as a grant target (root already bypasses the socket's
`0600` mode and needs no ACE). Per-user deployments have a single owner and
no ACL-managed set, so `operator grant`/`revoke` require the system daemon;
`operator list` still works everywhere, reading whatever ACL is present.

**Revocation takes effect at the next RPC on any connection, not just at
`connect()`.** A server-wide gRPC interceptor pair rechecks the caller's uid
against a fresh (uncached) ACL read on every unary call and every stream
open — uniformly, across all RPCs, so a new RPC is gated by default and no
per-handler allowlist can drift. A revoked operator holding an
already-open connection is denied on its very next call. The daemon also
actively kills that operator's in-flight streams (`WatchStatus`,
`StreamLogs`) the moment `runnyctl operator revoke` lands the ACL mutation,
via an event-driven per-uid cancel registry — no polling, no lingering
read stream. The killed stream ends with a `PermissionDenied` the client
can show, not a silent clean close. The platform's already-privileged
principals always pass — root on darwin, SYSTEM and elevated
Administrators on Windows — since they bypass the socket's access control
by design and hold no ACE; a peer whose identity could not be read is
denied (fail closed). Out-of-band ACL edits (a manual
`chmod`, or `install-daemon` resetting the ACL) are not swept for
in-flight streams — only the in-process revoke path triggers the kill —
but every new RPC still observes the edit immediately.

**The revocation gate arms only where a real peer-identity read exists,
proven at startup where the read is undocumented surface.** Fail-closed
only degrades gracefully when a peer identity is at least *sometimes*
readable; on a platform where it is *never* readable, arming the gate
would deny every RPC unconditionally rather than fail closed on the
exceptional case. On darwin the read is `SO_PEERCRED` (documented,
always-on kernel surface) and the gate simply arms. On Windows —
whose AF_UNIX implementation deliberately omits credential passing —
the read is the AF_UNIX getpeerpid ioctl resolved to the connecting
process token's SID, once per connection, server-side, never
client-forgeable; but that ioctl is undocumented, so the daemon proves it
before relying on it: at startup it dials its own socket and requires its
own PID and token-user SID back, and only a passing probe arms the gate.
A probe failure (a future Windows build changing the ioctl) is loud
feature loss, never lockout and never silent: one error log, and the
daemon falls back to the documented socket-is-the-sole-gate baseline
(above) with no live per-connection revoke. SYSTEM and elevated
Administrators pass the armed gate without holding an ACE and are refused
as grant targets — they hold Full ACEs from the install bootstrap's DACL
plus `SeTakeOwnershipPrivilege`, so denying them would be theater, the
same already-privileged rationale as root on darwin. The accepted
residual: Windows peer identity is PID-indirected (the ioctl attests the
peer PID; the SID comes from opening that process's token), unlike
darwin's direct per-connection kernel attestation, so a peer-exit/PID-reuse
window exists — exploitable only by a principal that already holds write
access to the socket (an operator, already past the authorization
boundary), against an audit trail documented as good-faith reconstruction;
the read happens at handshake, while the peer demonstrably just connected,
narrowing the window to microseconds. The remaining platforms (linux and
friends) have no peer-identity read wired up, so the gate stays unarmed
there on the same socket-is-the-sole-gate baseline.

The kill is cooperative, not preemptive: it cancels a context both stream
handlers already select on between sends, so a handler currently blocked
**inside** a send (a slow or non-reading client has exhausted HTTP/2 flow
control) is not interrupted mid-send — gRPC's `SendMsg` never consults the
cancelled context, only the handler's own `Context().Done()` case does.
That stream stays open until the client's connection actually ends, most
plausibly on `StreamLogs -follow`'s higher-volume path. Forcing an
in-flight send to abort would mean tearing down the client's whole
underlying connection, killing every other RPC that client has open too —
a broader blast radius than the narrow case it would close. Accepted as a
residual: the common case (a handler idling between sends) is closed; a
stalled reader mid-send is not.

## Ephemeral guests

Each cycle runs a **fresh APFS clone, destroyed on teardown** by the destroy-and-recycle
mechanism ([ADR-0004](architecture-decisions/0004-destroy-and-recycle-state-machine.md));
no state survives between jobs in a slot. Single-use isolation is the security
property here — distinct from the no-silent-failure property, whose mechanism
only supplies the destruction that enforces it. A job cannot leave credentials, artifacts, or a foothold for
the next occupant of its slot.

## GitHub authentication

Runners register with **just-in-time config minted from a short-lived
installation token** (App JWT → installation token → JIT config), never a
long-lived registration token
([ADR-0003](architecture-decisions/0003-jit-runner-config.md)). The JIT config
is single-use.

## Observability (OTLP egress)

`runnyd` makes outbound OTLP gRPC calls to a collector only when the operator
configures `observability.otlp.endpoint`; the default is fully off. See
[the observability architecture doc](architecture/observability.md) and
[ADR-0024](architecture-decisions/0024-observability-event-seam.md) for the
event seam this exports. Its security posture:

- **Off by default, opt-in only.** An absent endpoint installs no SDK, no
  goroutines, and makes no outbound connection — identical to a daemon built
  without `internal/telemetry` at all.
- **No listening socket.** Export is push-only, to the one collector the
  operator names. The daemon's attack surface gains no new inbound port.
- **Transport security follows the scheme.** `https://` selects TLS;
  `http://` is accepted only for a local/trusted collector on the operator's
  own network, the same rule `internal/home` enforces when it validates the
  config.
- **No credentials in the payload.** Traces and metrics carry cycle/slot
  identity and timing, not GitHub tokens, SSH credentials, or job secrets —
  the same boundary the cycle record and `runnyctl why` already respect.
  Guest-controlled strings (job names) are bounded on the record path before
  they can become telemetry, so they cannot inflate metric label
  cardinality.
- **Bounded, non-blocking.** Export uses the SDK's drop-on-queue-full
  batching — a slow or unreachable collector drops telemetry rather than
  stalling a cycle. Exporter errors are logged, never silent. Shutdown flush
  runs under a fixed wall-clock deadline, so a dead collector cannot turn
  daemon exit into a hang.

## App update check (outbound network call)

The Runny app makes one **first-party** outbound HTTPS call: the self-update
notify check (`AppUpdateChecker`) hits
`api.github.com/repos/bojanrajkovic/runny/releases/latest` — unauthenticated,
with a 10s timeout and no body upload. This is the **only** outbound call the
app initiates. Its security posture:

- **No credentials sent.** The request carries no token; GitHub's public
  `/releases/latest` endpoint requires none. Rate-limit (60/hr/IP, unauthenticated)
  is handled by returning `nil` on a non-200 — no retry storm, no backoff loop.
- **Fail-quiet by default.** Every failure condition — non-200, network error,
  unparseable tag, 0.0.0 unstamped build — returns `nil` silently. The check
  never surfaces a network error to the operator.
- **Bounded.** The `URLRequest` carries a `timeoutInterval: 10`; there is no
  retry. A hung or slow GitHub API stops the check, not the app.
- **Toggleable.** `Prefs.checkForAppUpdates` (default on) disables the check
  entirely via **Settings → Updates**. Off means zero outbound calls.
- **Notify only.** The app **never downloads, replaces, or patches itself**. The
  banner links to the GitHub releases page; actual upgrade goes through brew or a
  manual `.dmg` install.

## The app is non-privileged

The app raises **no** `administrator` prompt and runs no privileged helper, ever:
it owns only the user's `gui/` launchd domain. Installing the system daemon,
writing any system path, and installing the CLI to `/usr/local/bin` are all
`runnyctl`'s — the operator's — domain, never the app's
([privilege-boundary.md](architecture/privilege-boundary.md),
[ADR-0023](architecture-decisions/0023-app-non-privileged-boundary.md);
[ADR-0018](architecture-decisions/0018-bundled-app-distribution.md) already
rejected a root helper for the app surface). The app bundles `runnyd` and
`runnyctl` but does **not** install the CLI — `runnyctl` reaches the user's PATH
via the Homebrew tap formula, the Homebrew cask, or by running it from inside the bundle. The bundled
binaries are signed inside-out — the daemon carries
`com.apple.security.virtualization`, the CLI carries none, both asserted at build
time, so the CLI can never inherit the VM grant.

The app can also **install `runnyd` as a per-user LaunchAgent** via `SMAppService`
— and this is *not* a root operation. The agent registers into the user's own
login session (gated by the user's Login Items approval), the bundled plist is
covered by the signature and notarization staple (injected before signing), and
the only OS interactions beyond `SMAppService` are bounded `launchctl` calls
(`bootout`/`kickstart`/`print`) scoped to the user's own `gui/<uid>` domain. There
is **no privileged helper, no `SMJobBless`, no System Extension**
([ADR-0018](architecture-decisions/0018-bundled-app-distribution.md) rejects a
root helper for the entire app surface). The daemon's Local Network (TCC) grant is
keyed to the bundled `runnyd`'s own code identity — so the one-time prompt names
`runnyd` (not the app), and the grant persists across upgrades only while the
signing identifier stays stable. The bundled `runnyd` raises that prompt with no
embedded `NSLocalNetworkUsageDescription`: a signed bare binary run as a per-user
agent prompts on its own, and macOS supplies generic text, ignoring any embed.

The app installs that agent **only when it owns the daemon**. Before any spawn —
install, repair, start-at-login enable, kickstart — it computes an ownership
verdict (its own `SMAppService` self-status plus a single bounded `launchctl print
system/<canonical-label>` probe to detect an installed system LaunchDaemon) and
proceeds only for an unowned or app-owned daemon; an installed system daemon, or
any inconclusive probe, denies the spawn and routes to an observer banner. So the
app never registers a competing agent over a system daemon, and the only
TCC-prompting surface — the bundled `runnyd` actually
spawning — is reached only on the unowned/app-owned path, so an observer never
provokes a Local Network prompt. Detection is read-only: it spawns `launchctl
print` under a hard bound but mutates nothing.

## Headless system daemon

On a headless host runnyd runs as a **non-root system LaunchDaemon** under a
dedicated, hidden service account (`_runny`) — no login, no shell, no home
directory ([ADR-0020](architecture-decisions/0020-headless-system-daemon.md),
[deploy.md](deploy.md) "Headless system daemon"). The install is privileged
**once** (`sudo runnyctl install-daemon` creates the account, the home, and the
LaunchDaemon, then `launchctl bootstrap system`); the daemon then **runs
unprivileged** as `_runny` — strictly less privilege than a root LaunchDaemon. A launchd-started daemon
is auto-allowed Local Network access regardless of uid (Apple TN3179), so
reaching guests needs neither a GUI prompt nor root.

The daemon's entire state lives in `/Library/Application Support/runny`, owned by
`_runny` at `0700`, nothing world- or group-accessible, with a **dual inheriting
ACL** that is the access boundary:

- the **operator** account gets directory write + read — edit `config.yaml`,
  land the `private_key_path` App key, and read cycle artifacts without `sudo`,
  and reach the control socket (above);
- the **`_runny`** account gets read — so the daemon can read an operator-landed
  `0600` config and key it does not own.

The ACL overrides the `0700` POSIX mode for exactly those two principals (macOS
evaluates an allow ACE ahead of the mode), and is inherited onto every file the
daemon and operator create beneath the home; the socket stays `0600` plus that
inherited ACL. The installer's account creation, home ownership, and the two
ACEs are therefore load-bearing: a group-writable home or an over-broad ACE
would widen who can drive the daemon, and a missing `_runny` read ACE would
leave the daemon unable to read its own config and key.

Two operator-trust assumptions underlie this, stated here so "true" and
"stated" do not drift apart:

- **The home's safety rests on `/Library/Application Support` staying
  root-owned.** runny owns and locks down its own `runny/` subdirectory, but
  the standard macOS ownership of the parent is what stops a non-root principal
  from pre-creating or swapping the home out from under the installer.
- **`config.yaml` and `private_key_path` are operator-trust surfaces.** The
  daemon reads whatever path the operator configures — symlinks included — and
  does not constrain it to the home. This is consistent with the model: the
  operator who edits the config already holds the App key and can disable
  hardening, so a path that operator controls is not a new trust boundary.

The operator's grant is **write** (connecting to the control socket requires
write on the socket file, and that same inherited ACE covers every file under the
home), so the operator can also modify or delete the daemon-written audit records
(`operator-access.json`, cycle records). This is consistent with the model above
— the operator already holds the App key and can disable hardening, so the audit
trail is **visibility, not a tamper-proof tier** held against the operator; it
records actions for later review, it does not defend against the operator who
controls the daemon. Narrowing it is not possible without breaking the operator's
own socket access.

Uninstall **purges the home** (keeping only the `_runny` account), so no App key
is left at rest once the operator removes the daemon, and the verify-before-remove
step means uninstall never reports success over a still-running daemon
([deploy.md](deploy.md) "Headless system daemon").
