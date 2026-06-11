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
  why SLOT [-cycles N]
                      render SLOT's recent cycle timelines

SLOT accepts the bare slot name (loupe-1) or a full runner name as shown
by status and the GitHub runners page (<prefix>-loupe-1-<cycle>).
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
		_, err = c.client.Pause(ctx, &runnyv1.PauseRequest{Slot: slot})
		if err == nil {
			fmt.Fprintf(c.out, "%s pausing (takes effect after the current cycle)\n", slot)
		}
		return err
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

func (c *ctl) renderStatus(resp *runnyv1.GetStatusResponse) {
	fmt.Fprintf(c.out, "runnyd %s, up %s\n\n", resp.GetVersion(),
		durString(time.Since(resp.GetDaemonStarted().AsTime())))
	slots := append([]*runnyv1.SlotStatus{}, resp.GetSlots()...)
	sort.Slice(slots, func(i, j int) bool { return slots[i].GetSlot() < slots[j].GetSlot() })
	// RUNNER shows the GitHub-visible name of the live cycle's runner —
	// what the org runners page lists — falling back to the bare slot in
	// BACKOFF (no runner exists; the slot is still the recycle/pause handle).
	name := func(s *runnyv1.SlotStatus) string {
		if n := s.GetRunnerName(); n != "" {
			return n
		}
		return s.GetSlot()
	}
	w := len("RUNNER")
	for _, s := range slots {
		w = max(w, len(name(s)))
	}
	fmt.Fprintf(c.out, "%-*s %-13s %-9s %-15s %-22s %s\n", w, "RUNNER", "STATE", "FOR", "IP", "JOB", "NOTE")
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
		fmt.Fprintf(c.out, "%-*s %-13s %-9s %-15s %-22s %s\n",
			w, name(s), state,
			durString(time.Since(s.GetStateEntered().AsTime())),
			s.GetVm().GetIp(), trunc(job, 22), trunc(note, 60))
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
	fmt.Fprintf(c.out, "  image %s | started %s | total %s\n",
		trunc(rec.GetImageDigest(), 19),
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
	bad := 0
	for _, ch := range resp.GetChecks() {
		mark := "ok  "
		if !ch.GetOk() {
			mark, bad = "FAIL", bad+1
		}
		fmt.Fprintf(c.out, "%-28s %s %s\n", ch.GetName(), mark, ch.GetDetail())
	}
	if bad > 0 {
		return fmt.Errorf("%d check(s) failed", bad)
	}
	return nil
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
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
