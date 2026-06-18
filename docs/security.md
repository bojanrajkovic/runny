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
  carries a **provenance attestation**.
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

Authorization is the socket itself: the 0600 owner-only `runnyd.sock` is the
sole gate, deliberately — the socket owner already transitively holds
everything injection grants (the config that can set `ssh_hardening: off`, the
App key, the daemon binary). The audit trail is the accountability layer, not a
second authorization tier.

## Ephemeral guests

Each cycle runs a **fresh APFS clone that is destroyed on teardown**; no state
survives between jobs in a slot (crash-only,
[ADR-0004](architecture-decisions/0004-crash-only-state-machine.md)). A job
cannot leave credentials, artifacts, or a foothold for the next occupant of its
slot.

## GitHub authentication

Runners register with **just-in-time config minted from a short-lived
installation token** (App JWT → installation token → JIT config), never a
long-lived registration token
([ADR-0003](architecture-decisions/0003-jit-runner-config.md)). The JIT config
is single-use.

## The app's privileged surface

The app's only action that escalates to **administrator** is vending the CLI —
symlinking the bundled `runnyctl` to `/usr/local/bin/runnyctl` (installing the
daemon agent, below, is user-scoped and never asks for admin). It tries an
unprivileged `createSymbolicLink` first and escalates only when `/usr/local/bin`
is not user-writable, through **one** `with administrator privileges` shell line —
no privileged helper, no `SMJobBless`, no System Extension
([ADR-0018](architecture-decisions/0018-bundled-app-distribution.md) rejects a
root helper for this surface). The escalated command is minimal and TOCTOU-safe:
it re-checks the foreign guard at the moment of mutation (test-and-create, never
`ln -f`), so a `brew install` landing between the unprivileged plan and the
privileged write cannot be clobbered, and it re-points only a `Runny.app` symlink,
never a foreign file. The target is shell-quoted through the AppleScript layer so
a path with spaces or quotes cannot break out of the command, and the result is
confirmed by reading the link back from disk, never trusted from the privileged
step's exit code. The bundled binaries are signed inside-out — the daemon carries
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
verdict (its own `SMAppService` self-status plus a bounded `launchctl` probe of
the brew and canonical labels in the user's `gui/<uid>` domain) and proceeds only
for an unowned or app-owned daemon; a Homebrew-managed or manually-installed
daemon, or any inconclusive probe, denies the spawn and routes to an observer
banner. So the app never registers a competing agent over a foreign manager, and
the only TCC-prompting surface — the bundled `runnyd` actually spawning — is
reached only on the unowned/app-owned path, so an observer never provokes a Local
Network prompt. Detection is local, `gui/<uid>`-scoped, and read-only: it spawns
`launchctl print` under a hard bound but mutates nothing.
