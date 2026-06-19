// runnyctl is the control CLI for runnyd, speaking runny.v1 over the daemon's
// unix socket. It is a deliberately equal peer of RunnyBar (ADR-0006).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mattn/go-runewidth"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/bojanrajkovic/runny/internal/home"
	runnyv1 "github.com/bojanrajkovic/runny/proto/runny/v1"
)

// version is stamped by Bazel under --config=release (ADR-0010).
var version = "dev"

const usage = `runnyctl — control surface for runnyd

usage: runnyctl [-json] <command> [args]

commands:
  version             print the client version
  status              one-shot slot status
  watch               follow status transitions
  logs [SLOT] [-daemon] [-replay N] [-follow=false]
                      stream runner output (all slots, or just SLOT);
                      -daemon streams runnyd's own log instead
  recycle SLOT [-reason WHY] [-force]
                      destroy SLOT's current cycle and start fresh;
                      -force is required to recycle a DEBUG hold or to
                      cancel a RUNNING job
  debug SLOT [-pubkey FILE] [-hold DUR] [-reason TEXT]
                      inject FILE's public key (default ~/.ssh/id_ed25519.pub)
                      into SLOT's live guest. Idle (LISTENING): the slot
                      freezes in DEBUG — runner killed (verified), no jobs,
                      max-idle suspended. Running a job (JOB): the key is
                      installed immediately (the job is NOT touched) and the
                      slot holds in DEBUG when the job ends. Rerun with the
                      same key to extend; a new key to add. Release with
                      'runnyctl recycle SLOT -force' (auto-releases after
                      -hold; default/cap limits.max_debug_hold).
  pause SLOT          hold SLOT after its current cycle drains
  resume SLOT         release a paused SLOT
  reload [-reason WHY]
                      validate the config on disk; if valid, drain the
                      fleet (running jobs finish first) and restart
                      runnyd on it (clears operator pauses)
  why SLOT [-cycles N]
                      render SLOT's recent cycle timelines

SLOT accepts the bare slot name (mac-1) or a full runner name as shown
by status and the GitHub runners page (<prefix>-mac-1-<cycle>).
  doctor              run the daemon's validation checks
`

func main() {
	if err := run(); err != nil {
		// -h/-help on any subcommand surfaces as flag.ErrHelp (the per-command
		// flag sets discard their own output); print usage once and exit 0, the
		// same outcome the top-level `runnyctl -h` already produces.
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(os.Stderr, usage)
			return
		}
		fmt.Fprintln(os.Stderr, "runnyctl:", err)
		os.Exit(1)
	}
}

func run() error {
	jsonOut := flag.Bool("json", false, "emit protojson instead of human rendering")
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		return fmt.Errorf("a command is required")
	}

	dir, err := home.Resolve()
	if err != nil {
		return err
	}
	conn, err := grpc.NewClient("unix://"+dir.SocketPath(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	client := runnyv1.NewRunnyServiceClient(conn)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := &ctl{client: client, json: *jsonOut, out: os.Stdout, err: os.Stderr}
	// A skew warning before any command output, on every daemon-dialing path.
	// version is the one local command that must work without a daemon, so it is
	// excluded — it returns below before any RPC.
	if args[0] != "version" {
		c.warnSkew(ctx)
	}
	err = c.dispatch(ctx, args)
	// codes.Unavailable is overloaded: a transport/dial failure (the daemon was
	// never reached), a LIVE daemon's application response ("slot is not accepting
	// commands"), and a stream that broke after connecting all use it. Only the
	// first warrants the home-aware hint. The daemon "answered" if a stream
	// received a record (c.connected — survives the connection leaving Ready on a
	// mid-stream death) OR the connection is currently Ready (the one-shot
	// app-level case, daemon alive). Otherwise the bare error stands.
	if shouldHint(err, c.connected || conn.GetState() == connectivity.Ready) {
		return connHint(err, dir.SocketPath(), socketFileExists(dir.SocketPath()))
	}
	return err
}

// dispatch runs a single runnyctl command. args[0] is the command (guaranteed
// non-empty by run); the remainder are its arguments. Every command parses its
// own flag set via subFlags, so the global -json is honored after the
// subcommand too and an unknown trailing flag errors rather than being
// swallowed or exiting the process (issue #47).
func (c *ctl) dispatch(ctx context.Context, args []string) error {
	switch cmd, rest := args[0], args[1:]; cmd {
	case "version":
		fs, j := subFlags("version")
		if err := c.parseNoArgs(fs, j, rest); err != nil {
			return err
		}
		fmt.Fprintln(c.out, version)
		return nil
	case "status":
		fs, j := subFlags("status")
		if err := c.parseNoArgs(fs, j, rest); err != nil {
			return err
		}
		return c.status(ctx)
	case "watch":
		fs, j := subFlags("watch")
		if err := c.parseNoArgs(fs, j, rest); err != nil {
			return err
		}
		return c.watch(ctx)
	case "logs":
		fs, j := subFlags("logs")
		replay := fs.Int("replay", 50, "buffered lines to replay")
		follow := fs.Bool("follow", true, "keep following after the replay")
		daemon := fs.Bool("daemon", false, "stream the daemon's own log instead of runner output")
		// SLOT is optional here, so logs can't use slotArg (which requires
		// exactly one); validate the at-most-one rule over the same parser.
		positional, err := c.parseArgs(fs, j, rest)
		if err != nil {
			return err
		}
		if len(positional) > 1 {
			return fmt.Errorf("logs takes at most one SLOT argument")
		}
		slot := ""
		if len(positional) == 1 {
			slot = positional[0]
		}
		if *daemon && slot != "" {
			return fmt.Errorf("-daemon and a slot filter are mutually exclusive")
		}
		return c.logs(ctx, *replay, *follow, *daemon, slot)
	case "recycle":
		fs, j := subFlags("recycle")
		reason := fs.String("reason", "operator request", "reason recorded in the cycle")
		force := fs.Bool("force", false, "recycle a DEBUG hold, or cancel a RUNNING job")
		slot, err := c.slotArg(fs, j, rest)
		if err != nil {
			return err
		}
		return c.recycle(ctx, slot, *reason, *force)
	case "debug":
		fs, j := subFlags("debug")
		pubkey := fs.String("pubkey", "", "public key file (default ~/.ssh/id_ed25519.pub)")
		hold := fs.Duration("hold", 0, "auto-release after this long (0 = limits.max_debug_hold)")
		reason := fs.String("reason", "", "audit note")
		slot, err := c.slotArg(fs, j, rest)
		if err != nil {
			return err
		}
		return c.debug(ctx, slot, *pubkey, *hold, *reason)
	case "pause":
		fs, j := subFlags("pause")
		slot, err := c.slotArg(fs, j, rest)
		if err != nil {
			return err
		}
		return c.pause(ctx, slot)
	case "resume":
		fs, j := subFlags("resume")
		slot, err := c.slotArg(fs, j, rest)
		if err != nil {
			return err
		}
		_, err = c.client.Resume(ctx, &runnyv1.ResumeRequest{Slot: slot})
		if err == nil {
			fmt.Fprintf(c.out, "%s resumed\n", slot)
		}
		return err
	case "reload":
		fs, j := subFlags("reload")
		reason := fs.String("reason", "", "reason recorded in the daemon log and cycle records")
		wait := fs.Bool("wait", false, "follow the drain and confirm the respawn came up on this config")
		respawnTimeout := fs.Duration("respawn-timeout", 90*time.Second, "max wait for the respawn after the daemon exits")
		timeout := fs.Duration("timeout", 0, "optional hard cap on the entire wait (0 = none)")
		if err := c.parseNoArgs(fs, j, rest); err != nil {
			return err
		}
		if *wait {
			return c.reloadWait(ctx, *reason, defaultFollowOpts(*respawnTimeout, *timeout))
		}
		return c.reload(ctx, *reason)
	case "why":
		fs, j := subFlags("why")
		cycles := fs.Int("cycles", 1, "how many recent cycles")
		slot, err := c.slotArg(fs, j, rest)
		if err != nil {
			return err
		}
		return c.why(ctx, slot, *cycles)
	case "doctor":
		fs, j := subFlags("doctor")
		if err := c.parseNoArgs(fs, j, rest); err != nil {
			return err
		}
		return c.doctor(ctx)
	default:
		flag.Usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// subFlags builds a subcommand flag set. It uses ContinueOnError so a bad flag
// surfaces as an ordinary runnyctl error (printed once with the runnyctl:
// prefix, and unit-testable) instead of a bare os.Exit(2), and discards the
// flag package's own output so that error isn't also printed unprefixed. Every
// subcommand registers -json here, so the global flag is accepted after the
// command as well as before it (issue #47); fold the returned bool into c.json
// with useJSON once parsing succeeds.
func subFlags(name string) (*flag.FlagSet, *bool) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs, fs.Bool("json", false, "emit protojson instead of human rendering")
}

// useJSON folds a subcommand's trailing -json into the effective output mode,
// OR-ing it with the global flag already in c.json so -json works in either
// position.
func (c *ctl) useJSON(local *bool) { c.json = c.json || *local }

// parseNoArgs parses fs for a subcommand that takes no positional arguments,
// folding -json. A stray positional errors rather than being silently ignored.
func (c *ctl) parseNoArgs(fs *flag.FlagSet, j *bool, args []string) error {
	positional, err := c.parseArgs(fs, j, args)
	if err != nil {
		return err
	}
	if len(positional) != 0 {
		return fmt.Errorf("%s takes no arguments (got %q)", fs.Name(), positional[0])
	}
	return nil
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

// parseArgs parses fs allowing flags and positional arguments to interleave in
// any order. Go's flag package stops at the first positional, so a flag written
// after a SLOT (`recycle -force mac-1 -json`) would otherwise be lost; this
// re-parses past each positional, collecting them, so the trailing -json (and
// any other global flag) is honored wherever it sits (issue #47). It folds a
// trailing -json on success, so callers can't forget the fold.
func (c *ctl) parseArgs(fs *flag.FlagSet, j *bool, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
	c.useJSON(j)
	return positional, nil
}

// slotArg parses a command that requires exactly one SLOT — in any position
// relative to the flags — and folds a trailing -json.
func (c *ctl) slotArg(fs *flag.FlagSet, j *bool, args []string) (string, error) {
	positional, err := c.parseArgs(fs, j, args)
	if err != nil {
		return "", err
	}
	if len(positional) != 1 {
		return "", fmt.Errorf("exactly one SLOT argument is required")
	}
	return positional[0], nil
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
// app's grant card for the headless channel: DENIED (runnyd can't reach the
// guest subnet) is loud, UNKNOWN is a proactive heads-up that the grant is
// pending confirmation, and REACHABLE / UNSPECIFIED (old or non-darwin daemon)
// stay quiet so routine status output isn't cluttered.
func localNetworkNote(g runnyv1.LocalNetworkGrant) string {
	switch g {
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
	// Guard DEBUG and JOB: both need -force, and JOB additionally requires the
	// cancel_running_job consent flag — which runnyctl sets only after
	// OBSERVING JOB, so a send-time race can never cancel a job the operator
	// didn't see.
	cancelJob := false
	resp, err := c.client.GetStatus(ctx, &runnyv1.GetStatusRequest{})
	if err != nil {
		// The guard is best-effort client-side UX, but a plain recycle still
		// releases a DEBUG hold daemon-side (holdForDebug's CmdRecycle arm has
		// no -force check). Without -force we cannot tell whether this recycle
		// would destroy a held guest, so refuse rather than silently proceeding
		// — a status blip must not let an unintended recycle through. With
		// -force the operator has already consented to whatever shape the slot
		// is in, so proceed.
		if !force {
			return fmt.Errorf("cannot read slot status to check for a debug hold or running job (%w); pass -force to recycle anyway", err)
		}
	} else if st := findSlotStatus(resp, slot); st != nil {
		switch st.GetState() {
		case runnyv1.SlotState_SLOT_STATE_DEBUG:
			if !force {
				return fmt.Errorf("slot %s is holding for debug; pass -force to recycle (this destroys the held guest)", slot)
			}
		case runnyv1.SlotState_SLOT_STATE_JOB:
			if !force {
				job := st.GetJob().GetName()
				return fmt.Errorf("slot %s is running job %q (%s); recycling cancels it — pass -force",
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

func (c *ctl) debug(ctx context.Context, slot, pubkeyFile string, hold time.Duration, reason string) error {
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
	return nil
}

func (c *ctl) renderDebug(slot string, resp *runnyv1.InjectDebugKeyResponse) {
	conn := fmt.Sprintf("ssh %s@%s", resp.GetUser(), resp.GetIp())
	if resp.GetArmed() {
		fmt.Fprintf(c.out, "%s: debug key %s installed into a RUNNING job\n", slot, resp.GetFingerprint())
		fmt.Fprintln(c.out, "  the job now executes with an operator credential present — recorded in the cycle's audit trail")
		fmt.Fprintf(c.out, "  connect:  %s\n", conn)
		c.renderHostKeys(resp.GetHostKeys())
		fmt.Fprintln(c.out, "  hold:     starts when the job ends")
		fmt.Fprintf(c.out, "  release:  runnyctl recycle %s -force   ·   extend: re-run this command\n", slot)
		return
	}
	fmt.Fprintf(c.out, "%s: debug key %s installed; the slot is frozen in DEBUG\n", slot, resp.GetFingerprint())
	fmt.Fprintf(c.out, "  connect:  %s\n", conn)
	c.renderHostKeys(resp.GetHostKeys())
	if hu := resp.GetHoldUntil(); hu != nil {
		fmt.Fprintf(c.out, "  hold:     auto-releases %s (in %s)\n",
			hu.AsTime().Local().Format(time.RFC3339), durString(time.Until(hu.AsTime())))
	}
	fmt.Fprintf(c.out, "  release:  runnyctl recycle %s -force   ·   extend: re-run this command\n", slot)
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
	verdict := "✓ success"
	if rec.GetResult() != "success" {
		verdict = fmt.Sprintf("✗ failed in %s: %s", rec.GetFailureState(), rec.GetFailureError())
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
		case "deadline":
			fmt.Fprintf(c.out, "    %-13s %8s  DEADLINE EXCEEDED: %s\n", state, d, sr.GetError())
		default:
			fmt.Fprintf(c.out, "    %-13s %8s  ERROR: %s\n", state, d, sr.GetError())
		}
	}
	for _, k := range rec.GetInjectedKeys() {
		line := fmt.Sprintf("  debug key %-9s [%s] %s", k.GetOutcome(), k.GetState(), k.GetFingerprint())
		if r := k.GetReason(); r != "" {
			line += " — " + r
		}
		if e := k.GetError(); e != "" {
			line += ": " + e
		}
		fmt.Fprintln(c.out, line)
	}
	for _, a := range rec.GetArtifacts() {
		if dir := rec.GetCycleDir(); dir != "" {
			fmt.Fprintf(c.out, "  artifact: %s/%s\n", dir, a)
		} else {
			fmt.Fprintf(c.out, "  artifact: %s (in the cycle dir under ~/.runny/cycles)\n", a)
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
// when it shortens. runewidth.Truncate budgets by display width (not rune count)
// and reserves the ellipsis's own width, so a wide-rune job name can't over-run
// its cell and shift the columns to its right (issue #51); it is grapheme-cluster
// aware, so it won't split a combining-mark or ZWJ-emoji sequence mid-cluster.
func trunc(s string, n int) string {
	return widthCond.Truncate(s, n, "…")
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
// final segment render identically — `runnyctl -json status` carries the
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

// widthCond measures terminal display width with East-Asian-ambiguous runes
// pinned to one column (and emoji left neutral), so cell widths are deterministic
// regardless of the operator's locale env — runewidth otherwise derives the
// ambiguous-width setting from LANG/LC_*, which would make the same status table
// align differently on different hosts.
var widthCond = &runewidth.Condition{EastAsianWidth: false, StrictEmojiNeutral: false}

// cellWidth is a cell's terminal display width. Rune count is wrong for the JOB
// and NOTE columns, which carry GitHub-controlled job display names that can hold
// double-width (CJK) or zero-width runes (issue #51): a workflow `name:` reaches
// runny verbatim via the runner's "Running job:" line. runewidth maps each rune
// to its column count (0/1/2).
func cellWidth(s string) int { return widthCond.StringWidth(s) }

// pad right-pads s with spaces to display width w.
func pad(s string, w int) string {
	if d := w - cellWidth(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}
