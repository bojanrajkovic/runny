# ADR-0013: Per-cycle ephemeral SSH keys via in-band rotation

**Status:** Accepted (2026-06-11)

## Context

Guests authenticate runny over SSH with a baked password (`admin`/`admin` on
cirruslabs images). Issue #24 originally framed this as a guest-to-guest pivot
risk; #25 empirically disproved that premise — vmnet bridge isolation is
hardwired on in `VZNATNetworkDeviceAttachment`, so sibling guests cannot reach
each other at L2 regardless of credentials (see `docs/security.md`).

What remains of the exposure is the **host↔guest datapath**, whose documented
residual is ARP-spoof MITM. That residual is as much a *host-key-verification*
problem as a credential one: client keys alone do not stop a fake server from
accepting our authentication and harvesting what we send it — by the MINT_JIT
state, that payload is a GitHub credential. Any fix therefore needs **both**
ephemeral client keys and host-key pinning.

Two constraints shaped the mechanism:

- **Pre-boot delivery is inert on the published image fleet.** cirruslabs
  Linux images ship cloud-init with `datasource_list: [ None ]`
  (`/etc/cloud/cloud.cfg.d/99_cirruslabs.cfg`), so a NoCloud `cidata` seed is
  silently ignored on every image runny can pull today; macOS has no
  cloud-init at all. Seed delivery needs an operator-built derived image
  (#38).
- **Guests are ephemeral and in-process** (ADR-0008): a per-cycle key that
  lives only in daemon memory dies with the daemon, and the guest dies with
  it — there is no persistence problem to solve.

## Decision

**Rotate to a per-cycle ephemeral key in-band, on both OSes, by default.**

Per cycle, after AWAIT_SSH authenticates with the pool password — its one use
per cycle — a new **SECURE_SSH** state (own deadline, `deadlines.secure_ssh`):

1. mints an ed25519 keypair in memory (`crypto/ed25519` →
   `ssh.NewSignerFromKey`); the private key never touches disk;
2. captures **all** of the guest's host public keys (the host-key algorithm
   is negotiated per connection, so the pin set must cover whatever sshd may
   present);
3. installs the public key in `authorized_keys` and disables
   `PasswordAuthentication` + `KbdInteractiveAuthentication` via an
   `sshd_config.d` drop-in that **sorts first** (`00-runny.conf`) — sshd
   takes the first obtained value per keyword across lexically-ordered
   includes, and image fleets ship later-sorting auth drop-ins that would
   silently win otherwise (ubuntu cloud images: `50-cloud-init.conf` with
   `PasswordAuthentication yes`; macOS: `100-macos.conf`). Linux then reloads
   sshd (reload, never restart: the established session and listener
   survive); macOS needs no reload (launchd socket-activates sshd per
   connection, config read at spawn);
4. reconnects authenticated by the cycle key with the captured host keys
   pinned, then **proves the negative**: attempts password auth and requires
   rejection (retried across the reload's asynchrony). A guest that still
   takes the password fails the state loudly — config-precedence quirks can
   never report SECURE_SSH ok while the password lives. Only after that
   proof is the password session closed.

Properties the implementation enforces:

- **Ordering invariant: SECURE_SSH strictly precedes MINT_JIT.** The JIT
  config never exists while the guest still accepts password auth; everything
  worth stealing flows over the keyed, pinned channel.
- **Auth selection is exclusive, never fallback** (`internal/sshx`): a signer
  set means publickey is the only method attempted. A silent password
  fallback would un-fix the exposure while reporting success.
- **Default-on.** `ssh_hardening: rotate` is the default; `off` is an
  explicit opt-out (interactive debugging, images whose sshd cannot take the
  drop-in) that skips the state entirely. Failures are loud cycle failures
  into TEARDOWN.
- **Nearly no new image contract.** Rotation needs passwordless sudo (the
  provision scripts already require it), an `sshd_config.d` include (stock
  images ship it), and sshd ≥ 8.7 for the `KbdInteractiveAuthentication`
  keyword (current image fleets clear this; an older sshd rejects the
  drop-in at reload). An image missing any of these fails the rotate exec or
  the post-flip verification with the step named in the cycle record; the
  operator's remedy is `ssh_hardening: off`.

**Conceded: trust-on-first-use.** The pin is captured over the initial
password connection, so an *active* MITM present at first contact can proxy
through. That attacker — a compromised sibling or an untrusted multi-tenant
fleet — is already declared out of scope at current scale in
`docs/security.md` (the softnet non-adoption rationale); rotation closes the
standing-credential and fake-server-after-first-contact exposures, which is
what the current scale warrants. Pre-boot key delivery (#38) would close the
TOFU window too.

The state machine's living diagram, including SECURE_SSH, lives in
`docs/architecture/runnyd.md` ("The cycle").

## Rejected alternatives

- **NoCloud seed, pre-boot delivery** (#24's original design): inert on every
  published tart-format Linux image (datasources disabled at image build) and
  impossible on macOS. Right mechanism for operator-built images — deferred
  as #38, layered on an image-derivation pipeline, not rejected on merits.
- **Status-quo password**: a standing network credential plus an unverified
  server identity on the path that carries a GitHub credential. The network
  isolation argument that made it tolerable covers guest→guest only, not the
  host↔guest residual.
- **vsock guest agent** (tart-guest-agent over `tart exec`'s RPC): the
  strongest endpoint — no network authentication surface at all — but
  adopting it means an FSL-licensed component with a Competing-Use question
  for a tart replacement, impersonating tart's versioned virtio console port
  to the agent's suicide-check on macOS, and a control-channel rework on the
  scale of ADR-0002. Rejected, not deferred; new evidence (license
  conversion, agent check loosened) could reopen it.
- **Bake a key at image build**: never rotates, couples every host to one
  keypair, and needs the same image pipeline the seed path needs — strictly
  worse than #38 for the same cost.
