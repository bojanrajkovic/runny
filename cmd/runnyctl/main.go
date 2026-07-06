// runnyctl is the control CLI for runnyd, speaking runny.v1 over the daemon's
// unix socket. It is a deliberately equal peer of RunnyBar.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/alecthomas/kong"
	"golang.org/x/term"

	"github.com/rivo/uniseg"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/bojanrajkovic/runny/internal/cycle"
	"github.com/bojanrajkovic/runny/internal/home"
	"github.com/bojanrajkovic/runny/internal/oci"
	runnyv1 "github.com/bojanrajkovic/runny/proto/runny/v1"
)

// version is stamped by Bazel under --config=release.
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "runnyctl:", err)
		os.Exit(1)
	}
}

// newKong builds the runnyctl parser over the CLI grammar. Writers and the exit
// hook are injected so tests can capture output and suppress os.Exit; production
// passes the real stdio and os.Exit (kong prints --help and exits 0 itself,
// leaving only parse and command errors to return).
func newKong(cli *CLI, stdout, stderr io.Writer, exit func(int)) (*kong.Kong, error) {
	// No kong.UsageOnError(): it only fires via FatalIfErrorf, which exits with
	// kong's usage-error code (80). Returning the parse error to main() instead
	// keeps the daemon's uniform exit-1 contract; kong's own message already
	// lists the commands on a bad/absent one.
	return kong.New(
		cli,
		kong.Name("runnyctl"),
		kong.Description("control surface for runnyd, speaking runny.v1 over the daemon's unix socket"),
		kong.Writers(stdout, stderr),
		kong.Exit(exit),
	)
}

func run() error {
	cli := &CLI{}
	parser, err := newKong(cli, os.Stdout, os.Stderr, os.Exit)
	if err != nil {
		return err
	}
	// A parse error (unknown/absent command, bad flag, missing arg) returns to
	// main(), which prints "runnyctl: <err>" and exits 1 — the uniform error
	// contract. kong handles --help itself during Parse (prints help, exits 0).
	kctx, err := parser.Parse(os.Args[1:])
	if err != nil {
		return err
	}

	// Local privileged commands create/destroy the system daemon: they run as
	// root (sudo), never dial a daemon, and must branch before the client home +
	// gRPC setup below (which a not-yet-installed daemon has nothing to answer).
	// kong.Command() is the bare command name for these (no positional args), so
	// the two-case gate matches exactly.
	switch kctx.Command() {
	case "install-daemon", "uninstall-daemon":
		return kctx.Run()
	}

	dir, err := home.ResolveClient()
	if err != nil {
		return err
	}
	socketPath := dir.SocketPath()
	conn, err := grpc.NewClient("unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := &ctl{client: runnyv1.NewRunnyServiceClient(conn), json: cli.JSON, out: os.Stdout, err: os.Stderr}
	// A skew warning before any command output, on every daemon-dialing path.
	// version is the one command that must work without a daemon, so it is
	// excluded — its Run makes no RPC.
	if kctx.Command() != "version" {
		c.warnSkew(ctx)
	}
	kctx.BindTo(ctx, (*context.Context)(nil))
	kctx.Bind(c)
	err = kctx.Run()
	// codes.Unavailable is overloaded: a transport/dial failure (the daemon was
	// never reached), a LIVE daemon's application response ("slot is not accepting
	// commands"), and a stream that broke after connecting all use it. Only the
	// first warrants the home-aware hint. The daemon "answered" if a stream
	// received a record (c.connected — survives the connection leaving Ready on a
	// mid-stream death) OR the connection is currently Ready (the one-shot
	// app-level case, daemon alive). Otherwise the bare error stands.
	if shouldHint(err, c.connected || conn.GetState() == connectivity.Ready) {
		return connHint(err, socketPath, socketFileExists(socketPath))
	}
	return err
}

// shouldHint reports whether a command's terminal error warrants the home-aware
// connection hint: only a transport/dial failure (the daemon never answered this
// invocation), never an application-level Unavailable or a post-connection
// stream break. daemonAnswered folds the two "the daemon was reached" signals —
// a stream that received at least one record, and a currently-Ready connection
// (the one-shot app-level Unavailable case, daemon still alive). A daemon death
// mid-stream is excluded by the stream signal, since gRPC moves the connection
// out of Ready before Recv surfaces the error.
func shouldHint(err error, daemonAnswered bool) bool {
	return !daemonAnswered && status.Code(err) == codes.Unavailable
}

// connHint augments a transport/dial-time gRPC Unavailable with a home-aware
// hint naming the resolved socket, giving runnyctl the same diagnostic the app's
// connection card shows. socketExists distinguishes a missing socket (daemon
// down, or serving a different home) from a present-but-silent one (daemon hung
// or still starting). It assumes the caller has already decided to hint via
// shouldHint; a non-Unavailable or nil error still passes through unchanged so
// the function is total.
func connHint(err error, socketPath string, socketExists bool) error {
	if status.Code(err) != codes.Unavailable {
		return err
	}
	if socketExists {
		return fmt.Errorf("%w\n  hint: the socket at %s isn't answering — is runnyd hung or still starting?", err, socketPath)
	}
	return fmt.Errorf("%w\n  hint: no socket at %s — is runnyd running, or serving a different home?", err, socketPath)
}

// socketFileExists reports whether the daemon socket is present on disk. It only
// steers connHint's wording, so a stat race is harmless either way.
func socketFileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

type ctl struct {
	client runnyv1.RunnyServiceClient
	json   bool
	out    io.Writer
	err    io.Writer
	// connected latches true once a streaming command has received at least one
	// record — proof the daemon answered, so a later mid-stream Unavailable keeps
	// the bare error instead of the connection hint. A current-Ready check can't
	// see this: gRPC leaves Ready before the stream's terminal error surfaces.
	connected bool
}

func (c *ctl) emit(m proto.Message) error {
	b, err := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(m)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(c.out, string(b))
	return err
}

func (c *ctl) status(ctx context.Context) error {
	resp, err := c.client.GetStatus(ctx, &runnyv1.GetStatusRequest{})
	if err != nil {
		return err
	}
	if c.json {
		return c.emit(resp)
	}
	c.renderStatus(resp)
	return nil
}

// Free-text tail column widths: the JOB cell is padded to jobColWidth so NOTE
// lines up, and both are clamped so a long job name or failure string can't
// run the row off the terminal. NOTE is last and unpadded (it overflows
// gracefully), so its width only clamps.
const (
	jobColWidth  = 22
	noteColWidth = 60
)

// localNetworkNote returns an operator-facing line for the daemon's live Local
// Network (TCC) grant, or "" when no affordance is warranted. It mirrors the
// app's grant card for the headless channel: SELF_DAEMONIZED and DENIED (runnyd
// can't reach the guest subnet) are loud — but with DISTINCT remediations, since
// toggling the TCC grant cannot fix a mislaunched daemon — UNKNOWN is a proactive
// heads-up that the grant is pending confirmation, and REACHABLE / UNSPECIFIED
// (old or non-darwin daemon) stay quiet so routine status output isn't cluttered.
func localNetworkNote(g runnyv1.LocalNetworkGrant) string {
	switch g {
	case runnyv1.LocalNetworkGrant_LOCAL_NETWORK_GRANT_SELF_DAEMONIZED:
		// Distinct from DENIED: the fix is the launch context, not the TCC grant.
		return "local network: self-daemonized — launchd did not start runnyd, so macOS denies it guest access; toggling Local Network access won't help. Start it via launchd or run it in the foreground, never background it (see docs/deploy.md)"
	case runnyv1.LocalNetworkGrant_LOCAL_NETWORK_GRANT_DENIED:
		return "local network: DENIED — runnyd can't reach the guest subnet; grant it Local Network access (see docs/deploy.md)"
	case runnyv1.LocalNetworkGrant_LOCAL_NETWORK_GRANT_UNKNOWN:
		// UNKNOWN spans two causes — no vmnet interface yet (no guest has booted)
		// and a vmnet that is up but whose gateway probe timed out — so the note
		// states the consequence, not a guessed cause.
		return "local network: unconfirmed — runnyd can't yet confirm it reaches the guest subnet; if guests fail to connect, grant Local Network access (see docs/deploy.md)"
	default:
		return ""
	}
}

func (c *ctl) renderStatus(resp *runnyv1.GetStatusResponse) {
	fmt.Fprintf(c.out, "runnyd %s, up %s\n\n", resp.GetVersion(),
		durString(time.Since(resp.GetDaemonStarted().AsTime())))
	if note := localNetworkNote(resp.GetLocalNetworkGrant()); note != "" {
		fmt.Fprintf(c.out, "%s\n\n", note)
	}
	slots := append([]*runnyv1.SlotStatus{}, resp.GetSlots()...)
	sort.Slice(slots, func(i, j int) bool { return slots[i].GetSlot() < slots[j].GetSlot() })
	// The drain banner: why every slot is pausing/recycling, and which
	// slots the drain is still waiting on (anything not wedged and not
	// paused-in-BACKOFF — the stable states that cannot start a job).
	if d := resp.GetDraining(); d != "" {
		var waiting []string
		for _, s := range slots {
			if s.GetWedged() || (s.GetPaused() && s.GetState() == runnyv1.SlotState_SLOT_STATE_BACKOFF) {
				continue
			}
			waiting = append(waiting,
				s.GetSlot()+" ("+strings.TrimPrefix(s.GetState().String(), "SLOT_STATE_")+")")
		}
		banner := "DRAINING: " + d
		if len(waiting) > 0 {
			banner += " — waiting on: " + strings.Join(waiting, ", ")
		}
		fmt.Fprintf(c.out, "%s\n\n", banner)
	}
	// RUNNER shows the GitHub-visible name of the live cycle's runner —
	// what the org runners page lists — falling back to the bare slot in
	// BACKOFF (no runner exists; the slot is still the recycle/pause handle).
	name := func(s *runnyv1.SlotStatus) string {
		if n := s.GetRunnerName(); n != "" {
			return n
		}
		return s.GetSlot()
	}
	w := cellWidth("RUNNER")
	for _, s := range slots {
		w = max(w, cellWidth(name(s)))
	}
	wi := cellWidth("IMAGE")
	for _, s := range slots {
		wi = max(wi, cellWidth(imageCell(s.GetImage(), s.GetImageDigest())))
	}
	// Cells are padded to display width (not byte length: a clamped cell ends
	// in a multi-byte ellipsis) and joined by single spaces; NOTE is last and
	// unpadded so it overflows gracefully. IMAGE sits with the identity/guest
	// cluster, before the free-text tail (JOB, NOTE).
	row := func(runner, state, dur, ip, image, job, note string) {
		line := pad(runner, w) + " " + pad(state, 13) + " " + pad(dur, 9) + " " +
			pad(ip, 15) + " " + pad(image, wi) + " " + pad(job, jobColWidth) + " " + note
		fmt.Fprintln(c.out, strings.TrimRight(line, " "))
	}
	row("RUNNER", "STATE", "FOR", "IP", "IMAGE", "JOB", "NOTE")
	for _, s := range slots {
		state := strings.TrimPrefix(s.GetState().String(), "SLOT_STATE_")
		if s.GetPaused() {
			state += "*"
		}
		if s.GetWedged() {
			state = "WEDGED!"
		}
		job := ""
		if s.GetJob() != nil {
			job = s.GetJob().GetName()
		}
		note := s.GetLastFailure()
		if s.GetConsecutiveFailures() > 0 {
			note = fmt.Sprintf("%d consecutive failures; %s", s.GetConsecutiveFailures(), note)
		}
		if d := s.GetDetail(); d != "" {
			note = d // live annotation beats stale failure text
		}
		// A DEBUG hold shows its auto-release countdown; an armed JOB shows
		// that the hold will catch the corpse at job end (issue #39).
		if s.GetState() == runnyv1.SlotState_SLOT_STATE_DEBUG && s.GetDebugHoldExpires() != nil {
			note = "auto-releases in " + durString(time.Until(s.GetDebugHoldExpires().AsTime())) + "; recycle to release"
		}
		if s.GetDebugHoldArmed() {
			note = "debug hold armed (enters DEBUG at job end)"
		}
		// In BACKOFF the useful number is when the slot retries, not how long
		// it has already waited (the FOR column). Surface the remaining
		// backoff: backoff total minus time already in the state.
		if s.GetState() == runnyv1.SlotState_SLOT_STATE_BACKOFF {
			remaining := time.Duration(s.GetBackoffSeconds())*time.Second - time.Since(s.GetStateEntered().AsTime())
			if remaining > 0 {
				retry := "retry in " + durString(remaining)
				if note == "" {
					note = retry
				} else {
					note = retry + "; " + note
				}
			}
		}
		row(name(s), state,
			durString(time.Since(s.GetStateEntered().AsTime())),
			s.GetVm().GetIp(), imageCell(s.GetImage(), s.GetImageDigest()),
			trunc(job, jobColWidth), trunc(note, noteColWidth))
	}
	fmt.Fprintln(c.out, "\n(* = paused; STATE* holds in BACKOFF after the current cycle. WEDGED! = guest survived force-stop; the daemon restarts cold once idle)")
}

func (c *ctl) watch(ctx context.Context) error {
	stream, err := c.client.WatchStatus(ctx, &runnyv1.WatchStatusRequest{})
	if err != nil {
		return err
	}
	for {
		resp, err := stream.Recv()
		if err != nil {
			return streamErr(err)
		}
		c.connected = true // the daemon answered; a later mid-stream break keeps the bare error
		if c.json {
			if err := c.emit(resp); err != nil {
				return err
			}
			continue
		}
		fmt.Fprintf(c.out, "\x1b[2J\x1b[H") // clear; it's a watch
		c.renderStatus(resp)
	}
}

func (c *ctl) logs(ctx context.Context, replay int, follow, daemon bool, slot string) error {
	stream, err := c.client.StreamLogs(ctx, &runnyv1.StreamLogsRequest{
		Replay: uint32(replay), Follow: follow, Daemon: daemon, Slot: slot,
	})
	if err != nil {
		return err
	}
	for {
		line, err := stream.Recv()
		if err != nil {
			return streamErr(err)
		}
		c.connected = true // the daemon answered; a later mid-stream break keeps the bare error
		if c.json {
			if err := c.emit(line); err != nil {
				return err
			}
			continue
		}
		ts := line.GetTime().AsTime().Local().Format("15:04:05")
		// Runner output reads like a log tail: timestamp, owning slot, the
		// guest's line verbatim. The daemon's structured records keep the
		// level + sorted-attrs rendering.
		if !daemon {
			fmt.Fprintf(c.out, "%s %s │ %s\n", ts, line.GetAttrs()["slot"], line.GetMessage())
			continue
		}
		attrs := make([]string, 0, len(line.GetAttrs()))
		for k, v := range line.GetAttrs() {
			attrs = append(attrs, k+"="+v)
		}
		sort.Strings(attrs)
		fmt.Fprintf(c.out, "%s %-5s %s %s\n", ts, line.GetLevel(), line.GetMessage(), strings.Join(attrs, " "))
	}
}

func (c *ctl) recycle(ctx context.Context, slot, reason string, force bool) error {
	// Guard DEBUG and JOB: both need --force, and JOB additionally requires the
	// cancel_running_job consent flag — which runnyctl sets only after
	// OBSERVING JOB, so a send-time race can never cancel a job the operator
	// didn't see.
	cancelJob := false
	resp, err := c.client.GetStatus(ctx, &runnyv1.GetStatusRequest{})
	if err != nil {
		// The guard is best-effort client-side UX, but a plain recycle still
		// releases a DEBUG hold daemon-side (holdForDebug's CmdRecycle arm has
		// no --force check). Without --force we cannot tell whether this recycle
		// would destroy a held guest, so refuse rather than silently proceeding
		// — a status blip must not let an unintended recycle through. With
		// --force the operator has already consented to whatever shape the slot
		// is in, so proceed.
		if !force {
			return fmt.Errorf("cannot read slot status to check for a debug hold or running job (%w); pass --force to recycle anyway", err)
		}
	} else if st := findSlotStatus(resp, slot); st != nil {
		switch st.GetState() {
		case runnyv1.SlotState_SLOT_STATE_DEBUG:
			if !force {
				return fmt.Errorf("slot %s is holding for debug; pass --force to recycle (this destroys the held guest)", slot)
			}
		case runnyv1.SlotState_SLOT_STATE_JOB:
			if !force {
				job := st.GetJob().GetName()
				return fmt.Errorf("slot %s is running job %q (%s); recycling cancels it — pass --force",
					slot, job, durString(time.Since(st.GetStateEntered().AsTime())))
			}
			cancelJob = true
		}
	}
	_, err = c.client.Recycle(ctx, &runnyv1.RecycleRequest{Slot: slot, Reason: reason, CancelRunningJob: cancelJob})
	if err == nil {
		fmt.Fprintf(c.out, "%s recycling: %s\n", slot, reason)
	}
	return err
}

// findSlotStatus returns the status for the named slot (by slot name or runner
// name), or nil.
func findSlotStatus(resp *runnyv1.GetStatusResponse, name string) *runnyv1.SlotStatus {
	for _, s := range resp.GetSlots() {
		if s.GetSlot() == name || s.GetRunnerName() == name {
			return s
		}
	}
	return nil
}

func (c *ctl) debug(ctx context.Context, slot, pubkeyFile string, hold time.Duration, reason string, noExec bool) error {
	if pubkeyFile == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolving home dir for default pubkey: %w", err)
		}
		pubkeyFile = filepath.Join(home, ".ssh", "id_ed25519.pub")
	}
	keyBytes, err := os.ReadFile(pubkeyFile)
	if err != nil {
		return fmt.Errorf("reading %s: %w", pubkeyFile, err)
	}

	// The client deadline MUST outlast the daemon's handler wait, which is
	// config-derived (reconcile_interval + secure_ssh). A hardcoded value
	// shorter than it makes the daemon outlive the client: the operator is told
	// "nothing injected" while the FSM installs the key. Read the daemon's own
	// computed wait from GetStatus and add slack; fall back to a generous
	// default only when talking to a daemon that predates the field.
	clientWait := 135 * time.Second
	if st, err := c.client.GetStatus(ctx, &runnyv1.GetStatusRequest{}); err == nil {
		if w := st.GetInjectHandlerWait().AsDuration(); w > 0 {
			clientWait = w + 10*time.Second
		}
	}
	dctx, cancel := context.WithTimeout(ctx, clientWait)
	defer cancel()
	req := &runnyv1.InjectDebugKeyRequest{
		Slot: slot, PublicKey: string(keyBytes), Reason: reason,
	}
	if hold > 0 {
		req.Hold = durationpb.New(hold)
	}
	resp, err := c.client.InjectDebugKey(dctx, req)
	if err != nil {
		return err
	}
	if c.json {
		return c.emit(resp)
	}
	c.renderDebug(slot, resp)
	return c.execSSH(resp, noExec)
}

func (c *ctl) renderDebug(slot string, resp *runnyv1.InjectDebugKeyResponse) {
	conn := fmt.Sprintf("ssh %s@%s", resp.GetUser(), resp.GetIp())
	if resp.GetArmed() {
		fmt.Fprintf(c.out, "%s: debug key %s installed into a RUNNING job\n", slot, resp.GetFingerprint())
		fmt.Fprintln(c.out, "  the job now executes with an operator credential present — recorded in the cycle's audit trail")
		fmt.Fprintf(c.out, "  connect:  %s\n", conn)
		c.renderHostKeys(resp.GetHostKeys())
		fmt.Fprintln(c.out, "  hold:     starts when the job ends")
		fmt.Fprintf(c.out, "  release:  runnyctl recycle %s --force   ·   extend: re-run this command\n", slot)
		return
	}
	fmt.Fprintf(c.out, "%s: debug key %s installed; the slot is frozen in DEBUG\n", slot, resp.GetFingerprint())
	fmt.Fprintf(c.out, "  connect:  %s\n", conn)
	c.renderHostKeys(resp.GetHostKeys())
	if hu := resp.GetHoldUntil(); hu != nil {
		fmt.Fprintf(c.out, "  hold:     auto-releases %s (in %s)\n",
			hu.AsTime().Local().Format(time.RFC3339), durString(time.Until(hu.AsTime())))
	}
	fmt.Fprintf(c.out, "  release:  runnyctl recycle %s --force   ·   extend: re-run this command\n", slot)
}

func (c *ctl) renderHostKeys(keys []string) {
	if len(keys) == 0 {
		return
	}
	fmt.Fprintln(c.out, "  host keys (append to known_hosts to pin):")
	for _, k := range keys {
		fmt.Fprintf(c.out, "    %s\n", k)
	}
}

// execSSH replaces the current process with the system ssh binary, dropping
// the operator directly into the guest. It only fires on an interactive TTY;
// piped/scripted invocations keep the print-only behavior. When host keys are
// present in the response, they are written to a temp file and passed via
// -o UserKnownHostsFile + StrictHostKeyChecking=yes so the first connect is
// verified rather than TOFU. On success, syscall.Exec replaces the process so
// deferred cleanup never runs — the OS sweeps /tmp. On error, the defer fires
// and removes the file before returning to the caller.
func (c *ctl) execSSH(resp *runnyv1.InjectDebugKeyResponse, noExec bool) error {
	if noExec {
		return nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil // not an interactive TTY; print-only fallback
	}
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		return nil // no ssh binary on PATH; print-only fallback
	}
	argv := []string{"ssh"}
	if keys := resp.GetHostKeys(); len(keys) > 0 {
		f, err := os.CreateTemp("", "runny-knownhosts-*")
		if err != nil {
			return fmt.Errorf("writing known_hosts for verified connect: %w", err)
		}
		defer os.Remove(f.Name()) // runs only on error; syscall.Exec success replaces the process
		for _, k := range keys {
			if _, err := fmt.Fprintln(f, k); err != nil {
				f.Close()
				return fmt.Errorf("writing known_hosts for verified connect: %w", err)
			}
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("writing known_hosts for verified connect: %w", err)
		}
		argv = append(argv, "-o", "UserKnownHostsFile="+f.Name(), "-o", "StrictHostKeyChecking=yes")
	}
	argv = append(argv, fmt.Sprintf("%s@%s", resp.GetUser(), resp.GetIp()))
	return syscall.Exec(sshPath, argv, os.Environ())
}

func (c *ctl) why(ctx context.Context, slot string, cycles int) error {
	resp, err := c.client.Why(ctx, &runnyv1.WhyRequest{Slot: slot, Cycles: uint32(cycles)})
	if err != nil {
		return err
	}
	if c.json {
		return c.emit(resp)
	}
	if len(resp.GetCycles()) == 0 {
		fmt.Fprintf(c.out, "%s has no recorded cycles yet\n", slot)
		return nil
	}
	for _, rec := range resp.GetCycles() {
		c.renderCycle(rec)
		fmt.Fprintln(c.out)
	}
	return nil
}

func (c *ctl) renderCycle(rec *runnyv1.CycleRecord) {
	// Result — not Ending — gates the success verdict: runCycle always sets
	// both together, but why exists to surface failure truthfully, so a
	// desynced record must never render a false ✓. The failure error rides
	// along on every non-success verdict: for a recycle it carries the reason
	// the operator typed — the whole point of recording one.
	verdict := "✓ success"
	if rec.GetResult() != "success" {
		verdict = fmt.Sprintf("✗ failed in %s: %s", rec.GetFailureState(), rec.GetFailureError())
		switch rec.GetEnding() {
		case string(cycle.EndingRecycle):
			verdict = fmt.Sprintf("↻ recycled by operator in %s: %s", rec.GetFailureState(), rec.GetFailureError())
		case string(cycle.EndingShutdown):
			verdict = fmt.Sprintf("⏻ interrupted by daemon shutdown in %s: %s", rec.GetFailureState(), rec.GetFailureError())
		}
	}
	fmt.Fprintf(c.out, "cycle %s on %s — %s\n", rec.GetCycleId(), rec.GetSlot(), verdict)
	// The configured ref (intent) beside the resolved digest (truth). A
	// configured "@sha256:" pin is stripped from the intent half: like the
	// status cell, a sha256 here means "resolved this cycle", never an echoed
	// config pin — and since resolving a pin returns the pin, echoing it would
	// print the digest twice. Records written by older daemons carry no ref
	// and render digest-only; a cycle that failed before ENSURE_IMAGE resolved
	// carries ref-only.
	ref, _ := splitPin(rec.GetImage())
	img := ref
	if d := rec.GetImageDigest(); d != "" {
		short := "sha256:" + shortDigest(d)
		if img == "" {
			img = short
		} else {
			img = ref + " @ " + short
		}
	}
	fmt.Fprintf(c.out, "  image %s | started %s | total %s\n",
		img,
		rec.GetStarted().AsTime().Local().Format(time.RFC3339),
		durString(rec.GetFinished().AsTime().Sub(rec.GetStarted().AsTime())))
	if v := rec.GetRunnerVersion(); v != "" {
		fmt.Fprintf(c.out, "  runner %s\n", runnerVersionDisplay(v))
	}
	if rec.GetVm().GetIp() != "" {
		fmt.Fprintf(c.out, "  vm %s (%s)\n", rec.GetVm().GetIp(), rec.GetVm().GetMac())
	}
	if rec.GetJob() != nil {
		line := fmt.Sprintf("  job %q", rec.GetJob().GetName())
		if ks := rec.GetJob().GetOperatorKeys(); len(ks) > 0 {
			line += " · ran with operator key(s) " + strings.Join(ks, ", ")
		}
		fmt.Fprintln(c.out, line)
	}
	for _, sr := range rec.GetStates() {
		state := strings.TrimPrefix(sr.GetState().String(), "SLOT_STATE_")
		d := durString(sr.GetLeft().AsTime().Sub(sr.GetEntered().AsTime()))
		switch sr.GetOutcome() {
		case "ok":
			fmt.Fprintf(c.out, "    %-13s %8s  ok\n", state, d)
		case "warn":
			// State did its mandatory job; a best-effort cleanup left an orphan.
			fmt.Fprintf(c.out, "    %-13s %8s  warn: %s\n", state, d, sr.GetError())
		case "deadline":
			fmt.Fprintf(c.out, "    %-13s %8s  DEADLINE EXCEEDED: %s\n", state, d, sr.GetError())
		default:
			fmt.Fprintf(c.out, "    %-13s %8s  ERROR: %s\n", state, d, sr.GetError())
		}
	}
	for _, k := range rec.GetInjectedKeys() {
		line := fmt.Sprintf("  debug key %-9s [%s] %s", k.GetOutcome(), k.GetState(), k.GetFingerprint())
		// The has-bit matters: an absent uid (older daemon, non-darwin host, a
		// cred-read miss) omits the clause; a resolved-but-empty username
		// falls back to a bare uid rather than silently dropping the subject.
		if k.OperatorUid != nil {
			if u := k.GetOperatorUser(); u != "" {
				line += fmt.Sprintf(" · by %s (uid %d)", u, k.GetOperatorUid())
			} else {
				line += fmt.Sprintf(" · by uid %d", k.GetOperatorUid())
			}
		}
		if r := k.GetReason(); r != "" {
			line += " — " + r
		}
		if e := k.GetError(); e != "" {
			line += ": " + e
		}
		fmt.Fprintln(c.out, line)
	}
	for _, a := range rec.GetArtifacts() {
		note := ""
		if a == "debug-session.log" {
			note = " (ANSI stripped; plain text)"
		}
		if dir := rec.GetCycleDir(); dir != "" {
			fmt.Fprintf(c.out, "  artifact: %s/%s%s\n", dir, a, note)
		} else {
			fmt.Fprintf(c.out, "  artifact: %s (in the cycle dir under ~/.runny/cycles)%s\n", a, note)
		}
	}
}

func (c *ctl) doctor(ctx context.Context) error {
	resp, err := c.client.Doctor(ctx, &runnyv1.DoctorRequest{})
	if err != nil {
		return err
	}
	if c.json {
		return c.emit(resp)
	}
	if bad := c.renderChecks(resp.GetChecks()); bad > 0 {
		return fmt.Errorf("%d check(s) failed", bad)
	}
	return nil
}

// renderChecks prints the doctor check table and returns the failure count.
func (c *ctl) renderChecks(checks []*runnyv1.DoctorCheck) int {
	bad := 0
	for _, ch := range checks {
		mark := "ok  "
		if !ch.GetOk() {
			mark, bad = "FAIL", bad+1
		}
		fmt.Fprintf(c.out, "%-28s %s %s\n", ch.GetName(), mark, ch.GetDetail())
	}
	return bad
}

func (c *ctl) prune(ctx context.Context, apply bool) error {
	resp, err := c.client.Prune(ctx, &runnyv1.PruneRequest{Apply: apply})
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			return fmt.Errorf("daemon does not support prune — restart runnyd to pick up the new version")
		}
		return err
	}
	if c.json {
		if err := c.emit(resp); err != nil {
			return err
		}
		if len(resp.GetErrors()) > 0 {
			return fmt.Errorf("prune completed with errors")
		}
		return nil
	}
	items := resp.GetItems()
	if len(items) == 0 && len(resp.GetSkips()) == 0 && len(resp.GetErrors()) == 0 {
		fmt.Fprintln(c.out, "nothing to reclaim")
		return nil
	}
	// Group items by kind for a two-section display.
	var bundles, tarballs []*runnyv1.ReclaimItem
	for _, it := range items {
		if it.GetKind() == "image-bundle" {
			bundles = append(bundles, it)
		} else {
			tarballs = append(tarballs, it)
		}
	}
	verb := "would reclaim"
	if apply {
		verb = "reclaimed"
	}
	if len(bundles) > 0 {
		fmt.Fprintln(c.out, "image bundles:")
		for _, it := range bundles {
			fmt.Fprintf(c.out, "  %-12s  %s  (%s)\n", oci.HumanBytes(it.GetBytes()), it.GetLabel(), it.GetReason())
		}
	}
	if len(tarballs) > 0 {
		fmt.Fprintln(c.out, "runner tarballs:")
		for _, it := range tarballs {
			fmt.Fprintf(c.out, "  %-12s  %s  (%s)\n", oci.HumanBytes(it.GetBytes()), it.GetLabel(), it.GetReason())
		}
	}
	fmt.Fprintf(c.out, "%s %s\n", verb, oci.HumanBytes(resp.GetReclaimedBytes()))
	for _, sk := range resp.GetSkips() {
		fmt.Fprintf(c.out, "skip: %s — %s (kept intact)\n", sk.GetRef(), sk.GetReason())
	}
	for _, e := range resp.GetErrors() {
		fmt.Fprintf(c.out, "error: %s\n", e)
	}
	if len(resp.GetErrors()) > 0 {
		return fmt.Errorf("prune completed with errors")
	}
	return nil
}

func (c *ctl) pause(ctx context.Context, slot string) error {
	resp, err := c.client.Pause(ctx, &runnyv1.PauseRequest{Slot: slot})
	if err != nil {
		return err
	}
	fmt.Fprintf(c.out, "%s pausing (takes effect after the current cycle)\n", slot)
	if n := resp.GetNote(); n != "" {
		fmt.Fprintf(c.out, "note: %s\n", n)
	}
	return nil
}

func (c *ctl) reload(ctx context.Context, reason string) error {
	// The preflight re-runs every startup check synchronously; without this
	// line a slow registry reads as a hang.
	fmt.Fprintln(os.Stderr, "validating config against startup checks (network checks may take up to a minute)…")
	resp, err := c.client.Reload(ctx, &runnyv1.ReloadRequest{Reason: reason})
	if err != nil {
		return err
	}
	if c.json {
		if err := c.emit(resp); err != nil {
			return err
		}
		if !resp.GetAccepted() {
			return fmt.Errorf("reload refused")
		}
		return nil
	}
	return c.renderReload(resp)
}

func (c *ctl) renderReload(resp *runnyv1.ReloadResponse) error {
	sha := resp.GetConfigSha256()
	if len(sha) > 12 {
		sha = sha[:12]
	}
	warn := func() {
		for _, w := range resp.GetWarnings() {
			fmt.Fprintf(c.out, "warning: %s — %s\n", w.GetName(), w.GetDetail())
		}
	}
	if resp.GetAccepted() {
		// Did THIS call start the drain? The daemon answers authoritatively
		// (started_drain), so the CLI never reconstructs the daemon's internal
		// drain-reason format to guess — a guess that misfires on duplicate
		// reasons, an exit-held suffix, or any reword across a version skew.
		if !resp.GetStartedDrain() {
			fmt.Fprintf(c.out, "config validated (sha256 %s); daemon already draining (%s) — the respawn will apply this config\n",
				sha, resp.GetDraining())
			warn()
			return nil
		}
		fmt.Fprintf(c.out, "reload accepted: config validated (sha256 %s); draining %d slot(s)\n", sha, resp.GetSlotCount())
		fmt.Fprintln(c.out, "running jobs finish first — watch with `runnyctl watch`")
		fmt.Fprintln(c.out, "the daemon exits and respawns on the new config once idle")
		if paused := resp.GetOperatorPausedSlots(); len(paused) > 0 {
			fmt.Fprintf(c.out, "note: operator-paused slots resume after the respawn: %s\n", strings.Join(paused, ", "))
		}
		warn()
		return nil
	}
	warn()
	c.renderChecks(resp.GetFailedChecks())
	if d := resp.GetDraining(); d != "" {
		fmt.Fprintf(c.out, "WARNING: the daemon is already draining (%s) and the respawn WILL load this invalid config — fix ~/.runny/config.yaml before the drain converges, or the respawn will crash-loop (visible in launchd.err.log; diagnose with runnyd -doctor)\n", d)
	}
	return fmt.Errorf("reload refused: the new config failed validation; the running daemon is unchanged")
}

func streamErr(err error) error {
	if err == io.EOF {
		return nil
	}
	return err
}

func durString(d time.Duration) string {
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return d.Round(time.Second).String()
	case d < time.Hour:
		return d.Round(time.Second).String()
	default:
		return d.Round(time.Minute).String()
	}
}

// trunc clamps s to at most n display columns, appending a one-column ellipsis
// when it shortens. It budgets by display width (not rune count) over whole
// grapheme clusters, so a wide-rune job name can't over-run its cell and shift
// the columns to its right (issue #51), and a multi-rune cluster (a VS16/ZWJ
// emoji, or a base plus combining mark) is never split mid-cluster.
func trunc(s string, n int) string {
	if cellWidth(s) <= n {
		return s
	}
	budget := n - 1 // reserve one column for the ellipsis
	var b strings.Builder
	w := 0
	rest, state := s, -1
	for len(rest) > 0 {
		var cluster string
		var cw int
		cluster, rest, cw, state = uniseg.FirstGraphemeClusterInString(rest, state)
		if w+cw > budget {
			break
		}
		b.WriteString(cluster)
		w += cw
	}
	return b.String() + "…"
}

// imageCell renders the IMAGE column: the configured ref's last path
// segment, plus "@" + the resolved digest's first 12 hex iff the wire
// carried image_digest — i.e. iff ENSURE_IMAGE resolved this cycle. The
// digest half is never truncated (it is the distinguishing part); the ref
// half is clamped. A configured "@sha256:" pin is stripped from the cell:
// rendering it would make "resolved this cycle" and "config echoed,
// registry never reached" byte-identical on pinned fleets (a resolve of a
// pin always returns the pin), corrupting the cell's one rule. Registry
// and namespace are also dropped, so cross-registry refs with the same
// final segment render identically — `runnyctl --json status` carries the
// full ref and digest when that matters.
func imageCell(ref, digest string) string {
	name, _ := splitPin(ref)
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	// 25 lets canonical name:tag forms (e.g. macos-sequoia-xcode:16.4)
	// render whole — clamping would cut the tag, the distinguishing half of
	// the name on a mixed fleet.
	name = trunc(name, 25)
	if digest == "" {
		return name
	}
	return name + "@" + shortDigest(digest)
}

// splitPin separates a configured "@sha256:..." digest pin from an image ref,
// returning the ref without the pin and the pin's hex ("" if unpinned). A pin
// is config intent, not a resolved digest: both image views drop it so a
// sha256 on screen always means "resolved this cycle", never an echoed pin.
func splitPin(ref string) (name, pinHex string) {
	name, pinHex, _ = strings.Cut(ref, "@sha256:")
	return name, pinHex
}

// runnerVersionDisplay extracts the semver from an asset filename like
// "actions-runner-osx-arm64-2.320.0.tar.gz" → "2.320.0". Falls back to the
// full filename for any shape that doesn't match.
func runnerVersionDisplay(assetName string) string {
	s := strings.TrimSuffix(assetName, ".tar.gz")
	if i := strings.LastIndexByte(s, '-'); i >= 0 {
		return s[i+1:]
	}
	return assetName
}

// shortDigest renders a digest (with or without the "sha256:" algorithm
// prefix) as docker-style 12-hex — the one abbreviation both image views use.
func shortDigest(digest string) string {
	return shortHex(strings.TrimPrefix(digest, "sha256:"))
}

// shortHex is the docker-style 12-hex short form of a digest's hex.
func shortHex(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// cellWidth is a cell's terminal display width. Rune count is wrong for the JOB
// and NOTE columns, which carry GitHub-controlled job display names that can hold
// double-width (CJK), zero-width, or emoji-presentation runes (issue #51): a
// workflow `name:` reaches runny verbatim via the runner's "Running job:" line.
// uniseg measures monospace width per grapheme cluster — wide CJK and VS16/ZWJ
// emoji count as two columns, ambiguous-width runes (accented Latin, the
// ellipsis) as one — with a fixed policy independent of the operator's locale.
//
// Emoji width has no universal answer: terminals disagree, so no library is right
// for every one. uniseg follows the Unicode rules and matches the common cases,
// but a few presentation stragglers it sizes as one column — notably keycap
// sequences like "1️⃣" (digit + VS16 + U+20E3) — render as two on some terminals
// and can nudge a row's alignment. That residual is accepted: chasing each
// cluster class would mean hand-coding against a single terminal's rendering, and
// `runnyctl --json` is exact when precise output matters.
func cellWidth(s string) int { return uniseg.StringWidth(s) }

// pad right-pads s with spaces to display width w.
func pad(s string, w int) string {
	if d := w - cellWidth(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}
