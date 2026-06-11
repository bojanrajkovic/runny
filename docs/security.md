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
guest poisoning the bridge's ARP table to intercept a sibling's traffic) or
DHCP-pool exhaustion under heavy guest churn. The userspace packet filter
[softnet](https://github.com/openai/softnet) closes both, but runs as a
SUID-root helper holding the `com.apple.vm.networking` entitlement — a
privileged runtime dependency against the no-runtime-binary posture
([ADR-0008](architecture-decisions/0008-native-virtualization-framework.md)).
It is not adopted: the threats it defends require an untrusted multi-tenant
fleet, out of scope at current scale. Adopting it would be ADR-worthy.

## Guest access

Guest control is **SSH only**, through `x/crypto/ssh` with mandatory socket
deadlines — never an external `ssh`/`sshpass` binary
([ADR-0002](architecture-decisions/0002-x-crypto-ssh-with-socket-deadlines.md)).
Host keys are not verified because guests are ephemeral with fresh keys each
boot (`internal/sshx`).

Provisioning currently authenticates with a **baked password** — `admin`/`admin`,
the base image default. The network isolation above is what bounds this: the
password is reachable from the host datapath, not from sibling guests.
Replacing it with per-cycle ephemeral keys for Linux pools is tracked in
[#24](https://github.com/bojanrajkovic/runny/issues/24).

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
