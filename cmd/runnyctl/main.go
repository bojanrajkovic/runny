// runnyctl is the control CLI for runnyd, speaking runny.v1 over the daemon's
// unix socket. It is a deliberately equal peer of RunnyBar (ADR-0006).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/bojanrajkovic/runny/internal/home"
	runnyv1 "github.com/bojanrajkovic/runny/proto/runny/v1"
)

// version is stamped by Bazel under --config=release (ADR-0010).
var version = "dev"

const usage = `runnyctl — control surface for runnyd

usage: runnyctl [-home DIR] [-json] <command> [args]

commands:
  version             print the client version
  status              one-shot slot status
  watch               follow status transitions
  logs [SLOT] [-daemon] [-replay N] [-follow=false]
                      stream runner output (all slots, or just SLOT);
                      -daemon streams runnyd's own log instead
  recycle SLOT [-reason WHY]
                      destroy SLOT's current cycle and start fresh
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
		fmt.Fprintln(os.Stderr, "runnyctl:", err)
		os.Exit(1)
	}
}

func run() error {
	homeFlag := flag.String("home", "", "runny home dir (default $RUNNY_HOME or ~/.runny)")
	jsonOut := flag.Bool("json", false, "emit protojson instead of human rendering")
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		return fmt.Errorf("a command is required")
	}

	dir, err := home.Resolve(*homeFlag)
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

	c := &ctl{client: client, json: *jsonOut, out: os.Stdout}
	switch cmd, rest := args[0], args[1:]; cmd {
	case "version":
		fmt.Fprintln(c.out, version)
		return nil
	case "status":
		return c.status(ctx)
	case "watch":
		return c.watch(ctx)
	case "logs":
		fs := flag.NewFlagSet("logs", flag.ExitOnError)
		replay := fs.Int("replay", 50, "buffered lines to replay")
		follow := fs.Bool("follow", true, "keep following after the replay")
		daemon := fs.Bool("daemon", false, "stream the daemon's own log instead of runner output")
		_ = fs.Parse(rest)
		slot := fs.Arg(0)
		if *daemon && slot != "" {
			return fmt.Errorf("-daemon and a slot filter are mutually exclusive")
		}
		return c.logs(ctx, *replay, *follow, *daemon, slot)
	case "recycle":
		fs := flag.NewFlagSet("recycle", flag.ExitOnError)
		reason := fs.String("reason", "operator request", "reason recorded in the cycle")
		slot, err := slotArg(fs, rest)
		if err != nil {
			return err
		}
		return c.recycle(ctx, slot, *reason)
	case "pause":
		fs := flag.NewFlagSet("pause", flag.ExitOnError)
		slot, err := slotArg(fs, rest)
		if err != nil {
			return err
		}
		return c.pause(ctx, slot)
	case "resume":
		fs := flag.NewFlagSet("resume", flag.ExitOnError)
		slot, err := slotArg(fs, rest)
		if err != nil {
			return err
		}
		_, err = c.client.Resume(ctx, &runnyv1.ResumeRequest{Slot: slot})
		if err == nil {
			fmt.Fprintf(c.out, "%s resumed\n", slot)
		}
		return err
	case "reload":
		fs := flag.NewFlagSet("reload", flag.ExitOnError)
		reason := fs.String("reason", "", "reason recorded in the daemon log and cycle records")
		_ = fs.Parse(rest)
		if fs.NArg() != 0 {
			return fmt.Errorf("reload takes no positional arguments")
		}
		return c.reload(ctx, *reason)
	case "why":
		fs := flag.NewFlagSet("why", flag.ExitOnError)
		cycles := fs.Int("cycles", 1, "how many recent cycles")
		slot, err := slotArg(fs, rest)
		if err != nil {
			return err
		}
		return c.why(ctx, slot, *cycles)
	case "doctor":
		return c.doctor(ctx)
	default:
		flag.Usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func slotArg(fs *flag.FlagSet, rest []string) (string, error) {
	// Accept both "cmd SLOT -flag" and "cmd -flag SLOT" (Go's flag package
	// stops at the first non-flag arg, so slot-first needs special-casing).
	var slot string
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		slot, rest = rest[0], rest[1:]
	}
	if err := fs.Parse(rest); err != nil {
		return "", err
	}
	switch {
	case slot != "" && fs.NArg() == 0:
		return slot, nil
	case slot == "" && fs.NArg() == 1:
		return fs.Arg(0), nil
	default:
		return "", fmt.Errorf("exactly one SLOT argument is required")
	}
}

type ctl struct {
	client runnyv1.RunnyServiceClient
	json   bool
	out    io.Writer
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

func (c *ctl) renderStatus(resp *runnyv1.GetStatusResponse) {
	fmt.Fprintf(c.out, "runnyd %s, up %s\n\n", resp.GetVersion(),
		durString(time.Since(resp.GetDaemonStarted().AsTime())))
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

func (c *ctl) recycle(ctx context.Context, slot, reason string) error {
	_, err := c.client.Recycle(ctx, &runnyv1.RecycleRequest{Slot: slot, Reason: reason})
	if err == nil {
		fmt.Fprintf(c.out, "%s recycling: %s\n", slot, reason)
	}
	return err
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
	if rec.GetVm().GetIp() != "" {
		fmt.Fprintf(c.out, "  vm %s (%s)\n", rec.GetVm().GetIp(), rec.GetVm().GetMac())
	}
	if rec.GetJob() != nil {
		fmt.Fprintf(c.out, "  job %q\n", rec.GetJob().GetName())
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
	for _, a := range rec.GetArtifacts() {
		fmt.Fprintf(c.out, "  artifact: %s (in the cycle dir under ~/.runny/cycles)\n", a)
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

func trunc(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
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

// cellWidth is a cell's display width. All table content is ASCII except
// trunc's ellipsis (one column), so rune count is the display width.
func cellWidth(s string) int { return utf8.RuneCountInString(s) }

// pad right-pads s with spaces to display width w.
func pad(s string, w int) string {
	if d := w - cellWidth(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}
