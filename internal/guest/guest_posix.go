// This file holds the darwin/linux (POSIX) dialect: the rotate/scramble
// shell fragments, the provision scripts and their assembly helpers, the
// stop-runner and debug-recorder shell scripts. A declaration belongs here
// iff it is used only by the POSIX side of a dispatcher method in guest.go —
// see internal/guest/CLAUDE.md for the split's rationale and the windows
// dialect's own file.
package guest

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/bojanrajkovic/runny/internal/home"
)

// The rotation scripts install the per-cycle public key and shut password
// auth off. They rely on the image contract the provision scripts
// already demand — passwordless sudo, an sshd_config that includes
// sshd_config.d, sshd recent enough for KbdInteractiveAuthentication (8.7+).
// An image missing any of these fails the exec or the post-flip verification
// loudly, by design.
//
// The drop-in must sort FIRST in sshd_config.d: sshd takes the first
// obtained value per keyword, and Include globs expand in lexical order —
// stock images ship later-sorting drop-ins that would win over a 99- name
// (ubuntu cloud images: 50-cloud-init.conf with "PasswordAuthentication
// yes"; macOS: 100-macos.conf). 00- beats both; verifyPasswordAuthDead
// catches any image where even that loses.
const rotateScriptBase = `set -e
umask 077
mkdir -p "$HOME/.ssh"
echo '%s' >> "$HOME/.ssh/authorized_keys"
printf 'PasswordAuthentication no\nKbdInteractiveAuthentication no\n' | sudo tee /etc/ssh/sshd_config.d/00-runny.conf >/dev/null
`

// linux: reload, NOT restart — reload keeps the established session (this
// one) and the listener alive while re-reading config. Debian-family units
// are named ssh, RHEL-family sshd; try both.
const rotateScriptLinux = rotateScriptBase + `sudo systemctl reload ssh || sudo systemctl reload sshd
`

// darwin: no reload — launchd socket-activates sshd per connection, so each
// connection's sshd reads the config fresh at spawn.
const rotateScriptDarwin = rotateScriptBase

// scramblePasswordPlaceholder is substituted via strings.ReplaceAll, not a
// fmt verb — a plain substring swap needs no escaping discipline, matching
// provisionScript's __RUNNER_TARBALL__ placeholder below.
const scramblePasswordPlaceholder = "__RUNNY_SCRAMBLE_PASSWORD__"

// scrambleLineLinux / scrambleLineDarwin set a fresh, never-disclosed
// password for the just-authenticated account (ssh_hardening: scramble),
// appended to the rotate script so it lands in the same exec as
// the key install — one round-trip, one set -e failure path. A scramble
// failure aborts after PasswordAuthentication is already off, so it degrades
// to plain "rotate" behavior rather than a worse state.
//
// The username comes from `id -un` on the guest, not a Go-level value
// substituted in: the only thing interpolated into either line is the
// random password, so there is nothing here for a misconfigured SSHUser to
// inject into.
//
// Residual, both OSes: the whole rotate script — this line included — is
// delivered as one SSH exec, which sshd runs as `<shell> -c "<script>"`, so
// the password is live in that shell's argv (`ps`/`/proc/<pid>/cmdline`) for
// the exec's full duration, not just chpasswd's own process. Same residual
// class the JIT config already accepts on the guest side (StartRunner's
// comment, below) — accepted here because this runs during SECURE_SSH,
// before any operator debug key could exist to read it.
const scrambleLineLinux = `printf '%s:%s\n' "$(id -un)" '` + scramblePasswordPlaceholder + `' | sudo chpasswd
`

const scrambleLineDarwin = `sudo dscl . -passwd "/Users/$(id -un)" '` + scramblePasswordPlaceholder + `'
`

// captureHostKeys reads every host public key the guest may present during
// key exchange. All of them: the host-key algorithm is negotiated per
// connection, so the pin set must cover whatever sshd offers
// (sshx.Config.HostKeys). The .pub files are world-readable; no sudo.
// awk 1, not cat: cat concatenates, so a .pub missing its trailing newline
// would merge two keys into one line and one pin would silently vanish
// (ParseAuthorizedKey reads the second key as the first one's comment).
const captureHostKeys = `awk 1 /etc/ssh/ssh_host_*_key.pub`

// The provision scripts stage the runner and exec run.sh, per guest OS.
//
// The runner ALWAYS comes from our cache share, into a runny-owned dir —
// cirruslabs images ship a preinstalled ~/actions-runner whose version rots
// (a bundled v2.332.0 got "deprecated and cannot receive messages" from the
// broker), and JIT runners cannot self-update. Never trust the image's copy.
//
// The share is this cycle's own per-slot mount, holding exactly the one tarball
// it cloned before boot. The script still stages that EXACT tarball by basename
// (substituted for __RUNNER_TARBALL__), not a `ls | head -1` glob: defense in
// depth that keeps the on-disk record honest (the staged version matches the
// RunnerVersion recorded for the cycle) and the cache-miss diagnostic precise,
// rather than a lexical pick.
//
// Exit 78 (EX_CONFIG) = the mount is missing this tarball — a host-side
// problem the post-mortem will show verbatim.

// darwin: the share appears at the automount path (macOS automounts tagged
// virtiofs shares) or gets mounted explicitly by tag; handle both.
//
// An SSH exec is a non-login shell, so macOS hands it a minimal PATH
// (/usr/bin:/bin:/usr/sbin:/sbin) with /etc/zprofile (and path_helper) never
// sourced — which drops /usr/local/bin, where pkg installers like the AWS CLI
// symlink, and Homebrew. The runner inherits this PATH and passes it to every
// job step, so a step that installs a tool into /usr/local/bin then can't run
// it ("aws: command not found" right after a successful install). Rebuild the
// PATH a normal login session has, once, before launching the runner.
const provisionScriptDarwin = `set -e
eval "$(/usr/libexec/path_helper -s)"
[ -x /opt/homebrew/bin/brew ] && eval "$(/opt/homebrew/bin/brew shellenv)" || true
# Surface the guest clock: a stale RTC breaks runner registration with an
# opaque expired-token error.
echo "runny: provision-clock $(date -u +%Y-%m-%dT%H:%M:%SZ)"
CACHE="/Volumes/My Shared Files"
if [ ! -d "$CACHE" ]; then
  sudo mkdir -p /Volumes/runny-cache 2>/dev/null || true
  sudo mount_virtiofs runny-cache /Volumes/runny-cache 2>/dev/null || true
  CACHE="/Volumes/runny-cache"
fi
TARBALL="$CACHE/__RUNNER_TARBALL__"
if [ ! -f "$TARBALL" ]; then echo "runny: runner tarball __RUNNER_TARBALL__ not in cache share $CACHE" >&2; exit 78; fi
RUNNER_DIR="$HOME/runny-runner"
rm -rf "$RUNNER_DIR" && mkdir -p "$RUNNER_DIR" && cd "$RUNNER_DIR"
tar -xzf "$TARBALL"
exec ./run.sh --jitconfig "$(cat)"
`

// linuxProvisionPrelude is the clock tripwire shared by every linux variant
// (same reasoning as the darwin script's own copy).
const linuxProvisionPrelude = `set -e
echo "runny: provision-clock $(date -u +%Y-%m-%dT%H:%M:%SZ)"
`

// linuxProvisionBody is shared by every linux variant once CACHE is set:
// stage the exact tarball, extract it, and exec run.sh. Only how CACHE gets
// populated differs between variants (linuxCacheMount / linuxCachePushed
// below) — kept as the one thing that varies so the two variants can't drift
// out of sync on everything else, the way their error-message wording once did.
const linuxProvisionBody = `TARBALL="$CACHE/__RUNNER_TARBALL__"
if [ ! -f "$TARBALL" ]; then echo "runny: runner tarball __RUNNER_TARBALL__ not in cache $CACHE" >&2; exit 78; fi
RUNNER_DIR="$HOME/runny-runner"
rm -rf "$RUNNER_DIR" && mkdir -p "$RUNNER_DIR" && cd "$RUNNER_DIR"
tar -xzf "$TARBALL"
sudo ./bin/installdependencies.sh >/dev/null 2>&1 || true
exec ./run.sh --jitconfig "$(cat)"
`

// linuxCacheMount: explicit virtiofs mount; installdependencies.sh (in
// linuxProvisionBody) covers images missing libicu et al (idempotent,
// tolerated offline when deps exist).
const linuxCacheMount = `CACHE=/mnt/runny-cache
sudo mkdir -p "$CACHE"
mountpoint -q "$CACHE" || sudo mount -t virtiofs runny-cache "$CACHE"
`

// provisionScriptLinux: the live-share variant (darwin's virtiofs-equivalent
// on the guest side).
const provisionScriptLinux = linuxProvisionPrelude + linuxCacheMount + linuxProvisionBody

// runnerPushCacheDir is where PushRunnerTarball stages the tarball, relative
// to $HOME, when the boot backend has no live share device (windows host —
// see hcs_windows.go's NeedsRunnerPush doc comment for why). Under $HOME
// rather than /mnt like linuxCacheMount's CACHE: the push runs over the
// already-established SSH session as the same non-root user that owns its
// own home dir, so no sudo is needed to create it. linuxCachePushed derives
// its CACHE line from this constant rather than restating it, so the two
// can't drift the way they briefly could when both were separate literals.
const runnerPushCacheDir = "runny-cache"

// linuxCachePushed: no virtiofs-equivalent share device works from a bare
// compute system -- PushRunnerTarball stages the tarball at $HOME/runny-cache
// before this script runs, so there is no mount step here.
const linuxCachePushed = `CACHE="$HOME/` + runnerPushCacheDir + `"
`

// provisionScriptLinuxPushed: the pushed-cache variant (windows HOST, bare
// compute Linux guest — see hcsMachine.NeedsRunnerPush).
const provisionScriptLinuxPushed = linuxProvisionPrelude + linuxCachePushed + linuxProvisionBody

const runnerTarballPlaceholder = "__RUNNER_TARBALL__"

// runStartMarker is the line that launches the runner. It is the anchor guest
// env `export`s and guest_setup commands are injected before, so run.sh
// inherits them; pinned by TestProvisionScriptsPinRunMarker so a refactor
// can't silently move it.
const runStartMarker = "exec ./run.sh"

// guestEnvExports renders a pool's guest_env as shell `export` lines to prepend
// to the runner launch, so run.sh and every job step it spawns inherit them.
// Keys are emitted sorted (deterministic script bytes; they are already
// validated as env-var names at config load). Values are POSIX single-quote
// escaped — wrapped in '...' with each embedded ' rewritten as '\” — so any
// value (quotes, spaces, $) is inert in the shell. Empty input renders nothing,
// keeping provisioning byte-identical for a pool without guest_env.
func guestEnvExports(env map[string]string) string {
	if len(env) == 0 {
		return ""
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		esc := strings.ReplaceAll(env[k], "'", `'\''`)
		fmt.Fprintf(&b, "export %s='%s'\n", k, esc)
	}
	return b.String()
}

// guestSetupBlock renders a pool's guest_setup as newline-joined shell
// commands to run after the guest_env exports and before the runner launches.
// Entries are injected verbatim — they are commands, not identifiers, so
// (unlike guest_env keys) their content can't be validated beyond the
// non-empty check already done at config load. Empty input renders nothing,
// keeping provisioning byte-identical for a pool without guest_setup.
func guestSetupBlock(cmds []string) string {
	if len(cmds) == 0 {
		return ""
	}
	var b strings.Builder
	for _, cmd := range cmds {
		b.WriteString(cmd)
		b.WriteString("\n")
	}
	return b.String()
}

// runnerTarballRE constrains the POSIX tarball basename the daemon
// substitutes into the provision script. The name is daemon-resolved
// (GitHub's asset filename), not client input, but it crosses into a shell
// command string, so this is a trust-boundary guard: the charset carries no
// shell metacharacter and no `/`, so the validated name is inert inside the
// script's double-quoted "$CACHE/…".
var runnerTarballRE = regexp.MustCompile(`^[A-Za-z0-9._-]+\.tar\.gz$`)

// provisionScript renders the POSIX (darwin/linux) provision script for the
// exact tarball this cycle resolved. It refuses a name that does not match
// runnerTarballRE rather than risk staging a glob (silent wrong-version) or
// interpolating an unexpected string into the command — fail the cycle
// loudly instead. Never called for a windows guest: StartRunner dispatches
// windows to startRunnerWindows before reaching here, since the windows
// launch is a launcher hand-off plus a watcher session, not a single
// exec'd script.
func provisionScript(goos, runnerTarball string, env map[string]string, setup []string, needsPush bool) (string, error) {
	if goos == home.OSWindows {
		return "", errors.New("provisionScript: windows guests never take the POSIX provision path (see StartRunner)")
	}
	if !runnerTarballRE.MatchString(runnerTarball) {
		return "", fmt.Errorf("refusing to stage runner tarball with an unexpected name %q", runnerTarball)
	}
	// needsPush is the caller's vm.Machine.NeedsRunnerPush() value (true on
	// windows, see hcs_windows.go's doc comment): the same signal that gated
	// whether PushRunnerTarball ran before this script, so the tarball's
	// actual location and the script that looks for it can never disagree.
	linuxScript := provisionScriptLinux
	if needsPush {
		linuxScript = provisionScriptLinuxPushed
	}
	script, err := perOS(goos, provisionScriptDarwin, linuxScript)
	if err != nil {
		return "", err
	}
	script = strings.ReplaceAll(script, runnerTarballPlaceholder, runnerTarball)
	// Prepend the pool's guest_env exports, then its guest_setup commands, to
	// the runner launch: run.sh and every job step inherit the env, and setup
	// runs with it already in scope. Empty env/setup is a no-op (block == ""),
	// leaving the script byte-identical.
	block := guestEnvExports(env) + guestSetupBlock(setup)
	if block != "" {
		script = strings.Replace(script, runStartMarker, block+runStartMarker, 1)
	}
	return script, nil
}

// stopRunnerScript kills the runner LISTENER tree and proves it dead. Every
// process in that tree carries "--jitconfig <blob>" in argv; the [-]-bracket
// makes the ERE match "--jitconfig" without matching the pattern's own literal
// text. Exit 0 = proven dead; 1 = survived SIGKILL; 2 = verification tool
// failure. TERM→KILL at 3s, hard bound ~6s — inside secure_ssh (15s).
//
// Scope: the proof targets the listener (the job-eligibility surface).
// Runner.Worker and job-step processes do not reliably carry --jitconfig and
// may survive the pkill; a dead listener plus single-use JIT is the
// no-new-jobs guarantee. pkill/pgrep ship on both guest OSes.
const stopRunnerScript = `PAT='[-]-jitconfig'
alive() {
  pgrep -f "$PAT" >/dev/null 2>&1
  case $? in
    0) return 0 ;;
    1) return 1 ;;
    *) echo "runny: pgrep failed; cannot verify runner death" >&2; exit 2 ;;
  esac
}
pkill -TERM -f "$PAT" 2>/dev/null
i=0
while alive; do
  i=$((i+1))
  [ "$i" -eq 12 ] && pkill -KILL -f "$PAT" 2>/dev/null
  if [ "$i" -gt 24 ]; then echo "runny: runner still alive after SIGKILL" >&2; exit 1; fi
  sleep 0.25
done
`

// debugSessionLogFile is the path on the guest where the recorder writes and
// teardown reads back the operator's session log. A single constant keeps the
// recorder scripts and PullDebugSession in sync.
const debugSessionLogFile = "/tmp/runny-debug-session.log"

// debugRecorderDarwin / debugRecorderLinux are the /tmp/runny-record wrapper
// scripts written alongside an operator debug key. The wrapper forces every
// use of that key (interactive shell, non-interactive command, and direct
// reconnects after runnyctl debug exits) through script(1), appending all
// output to debugSessionLogFile for teardown to pull.
//
// The split mirrors provisionScriptDarwin / provisionScriptLinux: BSD script(1)
// uses a positional command form; util-linux uses -c. The fallback ensures an
// operator is never locked out when script is absent — record nothing rather
// than deny access.
const debugRecorderDarwin = "#!/bin/sh\n" +
	"if ! command -v script >/dev/null 2>&1; then\n" +
	"  if [ -n \"$SSH_ORIGINAL_COMMAND\" ]; then exec \"${SHELL:-/bin/sh}\" -c \"$SSH_ORIGINAL_COMMAND\"; fi\n" +
	"  exec \"${SHELL:-/bin/sh}\"\n" +
	"fi\n" +
	"if [ -n \"$SSH_ORIGINAL_COMMAND\" ]; then\n" +
	"  exec script -q -F -a " + debugSessionLogFile + " /bin/sh -c \"$SSH_ORIGINAL_COMMAND\"\n" +
	"else\n" +
	"  exec script -q -F -a " + debugSessionLogFile + "\n" +
	"fi\n"

const debugRecorderLinux = "#!/bin/sh\n" +
	"if ! command -v script >/dev/null 2>&1; then\n" +
	"  if [ -n \"$SSH_ORIGINAL_COMMAND\" ]; then exec \"${SHELL:-/bin/sh}\" -c \"$SSH_ORIGINAL_COMMAND\"; fi\n" +
	"  exec \"${SHELL:-/bin/sh}\"\n" +
	"fi\n" +
	"if [ -n \"$SSH_ORIGINAL_COMMAND\" ]; then\n" +
	"  exec script -q -f -a -c \"$SSH_ORIGINAL_COMMAND\" -e " + debugSessionLogFile + "\n" +
	"else\n" +
	"  exec script -q -f -a " + debugSessionLogFile + "\n" +
	"fi\n"

// installDebugKeyScript writes the per-OS session recorder to /tmp/runny-record,
// then appends a command=-wrapped authorized_keys line and greps back the full
// wrapped line to prove the command= wrapper landed (not just the bare key).
// The command= option forces every operator SSH session through the recorder
// regardless of what the client requests. restrict denies forwarding/X11/agent;
// pty re-grants the PTY restrict would otherwise deny (which script(1) needs).
// The daemon's own cycle key is a separate, unwrapped line, so daemon
// operations are unaffected.
//
// Format args: recorder-script-content, key-line, key-line.
const installDebugKeyScript = `set -e
umask 077
mkdir -p "$HOME/.ssh"
printf '%%s' '%s' > /tmp/runny-record
chmod 0755 /tmp/runny-record
printf '%%s\n' 'command="exec /tmp/runny-record",restrict,pty %s' >> "$HOME/.ssh/authorized_keys"
grep -qF -- 'command="exec /tmp/runny-record",restrict,pty %s' "$HOME/.ssh/authorized_keys"
`
