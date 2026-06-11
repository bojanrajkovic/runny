# Deploying runnyd

How runnyd is installed on a macOS host, how the macOS **Local Network (TCC)**
grant is validated — the one genuinely uncertain part — and the sand cutover.

## Why this is not just `launchctl load`

runnyd boots guests on Apple's NAT/vmnet network and reaches them over SSH on
the `192.168.64.0/24` subnet. macOS 15+ gates that behind the **Local Network**
privacy permission (TCC). The trap (e2e-discovered, recorded in
`docs/architecture/runnyd.md` sharp edges):

- A **system LaunchDaemon** or a **background-reparented** runnyd is *silently
  denied* vmnet access — every guest dial fails `connect: no route to host`
  while the host shell reaches the same address.
- A **foreground child of `sshd`** inherits sshd's exemption, which is why the
  interim workaround is "run it under a held SSH session."
- A **LaunchAgent in a GUI login session** can show the one-time Local Network
  prompt; once accepted, the grant should stick.

So runnyd installs as a **per-user LaunchAgent**, not a LaunchDaemon, and the
deployment is not validated until the grant is confirmed to survive a reboot
and an upgrade. The `runnyd -doctor` **`local-network`** check reports whether
this process can reach the guest subnet (it only asserts once a guest is up).

## Prerequisites

- macOS on Apple Silicon, logged into a **GUI session** for the first install
  (the laptop is the better venue for the TCC experiment than headless ix).
- `runnyd` codesigned with the `com.apple.security.virtualization` entitlement.
  Ad-hoc signing boots VMs fine (`tools/sign/runnyd.entitlements`, ADR-0008);
  Developer ID is only needed for distribution — see "Signature stability."
- `~/.runny/config.yaml` with at least one pool, valid GitHub App credentials,
  and the runner-administration permission (`runnyd -doctor` asserts it).

## Install (for testing, from this checkout)

```sh
RUNNYD=$(pwd)/bazel-bin/cmd/runnyd/runnyd_/runnyd \
  ./tools/deploy/install.sh        # writes the LaunchAgent, bootstraps it
runnyd -doctor                     # confirm the checks, including local-network
./tools/deploy/uninstall.sh        # tear down (leaves ~/.runny intact)
```

Run `install.sh` from a GUI session, not a bare SSH shell. The agent label is
`com.coderinserepeat.runnyd`; stop it with `launchctl bootout gui/$(id -u)/com.coderinserepeat.runnyd`,
never by killing the process (KeepAlive would respawn it — that is deliberate,
it is what makes the ADR-0012 wedge restart work).

## The Local Network grant — the validation that gates everything

Do this on the **laptop first** (it has a reliable GUI session), then replicate
on ix:

1. Install the LaunchAgent (above) and boot one guest (a one-pool config,
   `count: 1`). Watch for the macOS **"runnyd would like to find and connect to
   devices on your local network"** prompt — **accept it.**
2. Confirm a guest dial now succeeds: the cycle reaches `AWAIT_SSH` →
   `PROVISION` instead of failing `no route to host`, and `runnyd -doctor`
   reports `local-network ok` while the guest is up.
3. **Reboot. Does it still work** without re-prompting?
4. **Rebuild/reinstall runnyd and reload the agent. Does the grant survive**,
   or does macOS re-prompt / silently re-deny?

Record the answers in the cutover ticket (#2). Steps 3–4 are the make-or-break:
if the grant does not persist, the deployment is not ready regardless of the
rest.

### Signature stability (the sleeper dependency on #10)

TCC grants are keyed to the binary's **code signature**. An **ad-hoc signature
changes hash on every build**, so a `brew upgrade` may silently drop the grant
and re-deny vmnet access. If step 4 shows the grant does not survive an
upgrade, then **stable Developer ID signing (#10) becomes a prerequisite for a
durable deployment**, not merely a distribution nicety. Settle this during the
experiment — it decides whether #10 blocks the cutover or not.

## Production install (via the tap)

Once the grant story holds, install through the Homebrew tap (the formula
installs both `runnyd` and `runnyctl`, and its `service` block is the
LaunchAgent — same shape as `tools/deploy/`):

```sh
brew install bojanrajkovic/tap/runny
# write ~/.runny/config.yaml, then from a GUI login session, WITHOUT sudo
# (sudo would install a Local-Network-denied LaunchDaemon):
brew services start runny
```

The release workflow regenerates the formula from `tools/deploy/runny.rb.tmpl`
on every release and pushes it to the tap, authenticating as the **release
bot App** (the `RELEASER_APP_ID` variable + `RELEASER_APP_PRIVATE_KEY` secret) with a
short-lived installation token scoped to `homebrew-tap`; it no-ops until those
secrets exist. That App is deliberately *not* the runtime runner-registration
App — release/CI and prod-host/runner-admin are separate blast radii.
`tools/deploy/install.sh` remains the path for running a from-checkout build.

## sand cutover (#9)

On ix, with the grant validated:

1. Install runnyd and write `config.yaml` with the production org pool
   (`count: 2`, `os: darwin`). Keep sand's launchd plist on disk for rollback.
2. `runnyd -doctor` — every check green, including `runner-perm:` and (with a
   guest up) `local-network`.
3. Stop sand: `launchctl bootout` its job (do not delete its plist yet).
4. Start runnyd (`install.sh`), confirm its runners show **online** in the
   GitHub org runner list.
5. **Soak:** watch a few real jobs run end to end (pull → boot → provision →
   JIT-register → run → teardown), and `runnyctl why` any failures. Run
   alongside sand's plist staged for rollback.
6. Once satisfied, permanently disable sand's launchd job and archive its plist.

### Rollback

`./tools/deploy/uninstall.sh`, then re-bootstrap sand's plist. runnyd leaves no
durable state that interferes — `~/.runny/vms` is swept on every start, and JIT
runner registrations self-remove or are swept on the next cold start.
