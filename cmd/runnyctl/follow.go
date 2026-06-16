package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	runnyv1 "github.com/bojanrajkovic/runny/proto/runny/v1"
)

// followOpts bounds reload follow mode with three clocks, never summed and at
// most one armed at a time, each sized to its phase's healthy magnitude with
// flat margin (a backstop, not a target):
//   - establishTimeout (Clock A): a unix RPC answers in milliseconds; a
//     connected-but-wedged socket fails fast here instead of hanging.
//   - reloadTimeout (Clock A'): a healthy preflight is O(seconds); this is
//     generous margin over a slow-but-healthy one, NOT the sum of the daemon's
//     per-check budgets. Exceeding it means the preflight itself is wedged.
//   - respawnTimeout (Clock B): a healthy cold start is O(seconds); this caps
//     the wait for a new process AFTER the daemon disappears, so a long healthy
//     drain never trips it.
//
// The drain itself is progress-bounded by stallWindow, never wall-clock-capped:
// a running job can hold the fleet for hours and the daemon refuses a drain
// budget on purpose (a budget could only fire by killing a job). overallTimeout
// is an optional end-to-end cap.
type followOpts struct {
	establishTimeout time.Duration // Clock A: stream dial + first snapshot
	reloadTimeout    time.Duration // Clock A': the Reload RPC
	stallWindow      time.Duration // FOLLOW: max time with no drain_seq progress
	respawnTimeout   time.Duration // Clock B: max wait for a new boot_id after disappearance
	overallTimeout   time.Duration // optional end-to-end cap (0 = none)
	pollInterval     time.Duration // AWAIT-RESPAWN / PROBE poll cadence
	redrawInterval   time.Duration // FOLLOW live-timer redraw cadence
}

// probeCallTimeout bounds each disambiguation GetStatus. Short: a live daemon
// answers fast and a down socket fails fast, so this only caps a wedged accept.
const probeCallTimeout = 2 * time.Second

func defaultFollowOpts(respawnTimeout, overallTimeout time.Duration) followOpts {
	return followOpts{
		establishTimeout: 5 * time.Second,
		reloadTimeout:    90 * time.Second,
		// WatchStatus pushes a snapshot per drain event plus a 30s heartbeat;
		// 90s is three missed events with no progress — a daemon gone silent
		// (hung), distinct from a slow-but-healthy drain that keeps bumping
		// drain_seq or is parked on a job.
		stallWindow:    90 * time.Second,
		respawnTimeout: respawnTimeout,
		overallTimeout: overallTimeout,
		pollInterval:   500 * time.Millisecond,
		redrawInterval: time.Second,
	}
}

// baseline identifies the process that accepted the reload, so a later status
// reporting a DIFFERENT process is the respawn. boot_id is the positive
// discriminator and rides the Reload response, so there is no pre-RPC read whose
// process could differ (closing the sub-RPC identity race). started is only the
// fallback for a pre-2 daemon that reports no boot_id.
type baseline struct {
	bootID  string
	started time.Time
}

// isSuccessor reports whether st is a genuinely new process. It prefers boot_id
// (positive identity) when both sides speak it; otherwise it falls back to a
// changed daemon_started — covering both a pre-2 daemon we baselined and a
// respawn that downgraded to a pre-2 binary mid-reload (which reports no
// boot_id, so a boot_id-only check would never recognize it and the wait would
// time out on a daemon that is in fact up).
func (b baseline) isSuccessor(st *runnyv1.GetStatusResponse) bool {
	if b.bootID != "" && st.GetBootId() != "" {
		return st.GetBootId() != b.bootID
	}
	// Guard the unset timestamp: a nil converts to the Unix epoch (not Go's
	// zero time), which would never equal b.started and falsely read as a
	// respawn.
	started := st.GetDaemonStarted()
	return started != nil && !started.AsTime().Equal(b.started)
}

// errStalled marks a daemon that stopped making drain progress without the
// socket dropping — the hung case the stall bound exists to catch, once the
// legitimately-long and held cases are ruled out.
var errStalled = errors.New("daemon stopped making drain progress (no advance within the stall window) and nothing long-running explains it; it may be hung — check `runnyctl status`")

// reloadWait runs the reload and follows it to convergence. Success is never
// "the socket came back": it is a genuinely new process (a changed boot_id)
// serving the exact config hash the reload returned. The status stream is opened
// BEFORE the reload so the early drain is never missed and the first snapshot
// doubles as the establish check (Clock A) and the pre-2 fallback baseline.
func (c *ctl) reloadWait(ctx context.Context, reason string, opts followOpts) error {
	if opts.overallTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.overallTimeout)
		defer cancel()
	}
	// Ctrl-C stops following without cancelling the drain; handle it so the
	// operator is told the drain continues daemon-side, not killed.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()

	reader, closeStream, first, err := c.openStream(ctx, opts.establishTimeout)
	if err != nil {
		return fmt.Errorf("daemon did not answer the status stream within %s; it may be wedged — check `runnyctl status`: %w",
			durString(opts.establishTimeout), err)
	}
	// follow owns the stream lifecycle from here (it reopens across transient
	// drops); this defer is the backstop for the early returns below, before
	// follow is reached. Double-cancel is a no-op.
	defer closeStream()
	base := baseline{started: first.GetDaemonStarted().AsTime()}

	fmt.Fprintln(os.Stderr, "validating config against startup checks (network checks may take up to a minute)…")
	rctx, rcancel := context.WithTimeout(ctx, opts.reloadTimeout)
	resp, err := c.client.Reload(rctx, &runnyv1.ReloadRequest{Reason: reason})
	rcancel()
	if err != nil {
		if errors.Is(context.Cause(rctx), context.DeadlineExceeded) && ctx.Err() == nil {
			return fmt.Errorf("the reload preflight did not finish within %s; the daemon may be wedged — check `runnyctl status`", durString(opts.reloadTimeout))
		}
		return err
	}
	if !resp.GetAccepted() {
		if c.json {
			_ = c.emit(resp)
			return fmt.Errorf("reload refused")
		}
		return c.renderReload(resp) // prints the failed checks, returns the refusal error
	}
	// Accepted summary: print now in human mode; in JSON mode hold it for the
	// single wrapper document at the end (two top-level JSON objects on one
	// stream is not a valid document).
	if !c.json {
		_ = c.renderReload(resp)
	}
	base.bootID = resp.GetAcceptingBootId()
	wantSHA := resp.GetConfigSha256()
	if base.bootID == "" && !c.json {
		fmt.Fprintln(os.Stderr, "warning: this daemon predates boot-id reporting (protocol < 2); confirming the respawn by start time only — a respawn racing this reload can't be told apart, and a stalled drain can't be detected (no drain-progress signal before protocol 2)")
	}

	st, msg, verr := c.follow(ctx, reader, closeStream, first, base, wantSHA, opts)

	if c.json {
		// One JSON document for the whole wait. Propagate an emit failure only
		// when there is no stronger verdict error.
		emitErr := c.emitReloadWait(resp, st)
		if verr != nil {
			return verr
		}
		return emitErr
	}
	if verr != nil {
		return verr
	}
	if msg != "" {
		fmt.Fprintln(c.out, msg)
	}
	return nil
}

// followState carries the progress-stall and verdict context ACROSS stream
// reopens. Anchoring the stall on drain_seq progress here, rather than re-arming
// a fresh timer per stream, is what stops a daemon that flaps its stream just
// under the stall window from resetting the bound forever — the stall accrues on
// real drain progress, not on stream liveness.
type followState struct {
	stall       *time.Timer
	haveSeq     bool
	lastSeq     uint64
	last        *runnyv1.GetStatusResponse // last old-process snapshot (carve-out + render)
	jobInFlight bool                       // a job was in flight in `last` (colors the verdict)
	start       time.Time
}

// fold records one old-process snapshot into the follow state: it becomes the
// carve-out/render snapshot, sets the job-in-flight flag, and resets the stall
// ONLY on a drain_seq change (so a flapping stream that makes no progress cannot
// keep re-arming the bound).
func (c *ctl) fold(snap *runnyv1.GetStatusResponse, fs *followState, opts followOpts) {
	fs.last = snap
	fs.jobInFlight = anySlotInState(snap, runnyv1.SlotState_SLOT_STATE_JOB)
	if seq := snap.GetDrainSeq(); !fs.haveSeq || seq != fs.lastSeq {
		fs.haveSeq, fs.lastSeq = true, seq
		resetTimer(fs.stall, opts.stallWindow)
	}
	c.renderFollow(snap, fs.start)
}

// absorb processes one snapshot from a live or reopened stream — both of which
// run AFTER the reload was accepted, so either can be the moment the successor
// first appears. It returns the snapshot when it is that successor; otherwise it
// is old-process state, folded into the follow state, and nil is returned. The
// pre-acceptance establish snapshot is deliberately NOT routed here — it can
// never be the successor (see follow).
func (c *ctl) absorb(snap *runnyv1.GetStatusResponse, base baseline, fs *followState, opts followOpts) *runnyv1.GetStatusResponse {
	if base.isSuccessor(snap) {
		return snap
	}
	c.fold(snap, fs, opts)
	return nil
}

// follow drives FOLLOW → (on a dropped stream) PROBE → AWAIT-RESPAWN, reopening
// the stream across a transient drop, until a new process answers or the wait
// is cancelled/stalled. `first` is the establish snapshot already consumed from
// the stream. It returns the respawn's status (for the JSON wrapper and the
// config-drift verdict), the human verdict message, and a verdict or operational
// error.
func (c *ctl) follow(ctx context.Context, reader *snapshotReader, closeStream func(), first *runnyv1.GetStatusResponse, base baseline, wantSHA string, opts followOpts) (*runnyv1.GetStatusResponse, string, error) {
	// Own the stream lifecycle: the closure reads the latest closeStream value,
	// so the CURRENT stream is the one cancelled on every return — fixing the
	// leak where a reopened stream outlived a caller-held stale cancel.
	defer func() { closeStream() }()
	fs := &followState{stall: time.NewTimer(opts.stallWindow), start: time.Now()}
	defer fs.stall.Stop()

	verdict := func(st *runnyv1.GetStatusResponse) (*runnyv1.GetStatusResponse, string, error) {
		msg, verr := respawnVerdict(st, wantSHA, fs.jobInFlight)
		return st, msg, verr
	}
	// The establish snapshot was captured BEFORE the reload was accepted, so it
	// can never be the respawn: the respawn does not exist until the accepting
	// process drains and exits. Seed the follow state from it only when it is the
	// accepting process itself; a DIFFERENT identity here is a predecessor
	// surfaced by a launchd restart that raced the reload (boot_id A on the wire,
	// boot_id B accepting). Routing it through absorb would mistake that
	// predecessor for the respawn and report convergence or drift against the
	// wrong process before the real drain even begins. The post-acceptance stream
	// (and any reopen) carries the genuine successor.
	if !base.isSuccessor(first) {
		c.fold(first, fs, opts)
	}
	for {
		err := c.streamDrain(ctx, reader, base, opts, fs)
		if ctx.Err() != nil {
			return nil, "", c.cancelled(ctx)
		}
		if errors.Is(err, errStalled) {
			return nil, "", err
		}
		// The stream ended; streamDrain never reports a successor (one cannot
		// appear on an established stream). Probe to tell a transient drop (old
		// daemon still draining) from the daemon actually exiting.
		switch p := c.probeDaemon(ctx, base, opts); p.kind {
		case probeNewProcess:
			return verdict(p.st)
		case probeStillOld:
			// Transient drop: reopen the stream (Clock A bounds its first
			// snapshot) and resume the still-healthy drain. The stall keeps
			// accruing across this reopen — only a drain_seq change resets it.
			nr, nclose, nfirst, err := c.openStream(ctx, opts.establishTimeout)
			if err != nil {
				if ctx.Err() != nil {
					return nil, "", c.cancelled(ctx)
				}
				return c.respawnOrCancel(ctx, base, wantSHA, fs.jobInFlight, opts)
			}
			closeStream()
			reader, closeStream = nr, nclose
			if st := c.absorb(nfirst, base, fs, opts); st != nil {
				return verdict(st)
			}
			continue
		case probeGone:
			return c.respawnOrCancel(ctx, base, wantSHA, fs.jobInFlight, opts)
		}
	}
}

// respawnOrCancel runs the hard-capped respawn wait and maps an interrupt or
// overall-timeout cancellation to the "stopped following" note.
func (c *ctl) respawnOrCancel(ctx context.Context, base baseline, wantSHA string, jobInFlight bool, opts followOpts) (*runnyv1.GetStatusResponse, string, error) {
	st, msg, verr := c.awaitRespawn(ctx, base, wantSHA, jobInFlight, opts)
	if ctx.Err() != nil {
		return nil, "", c.cancelled(ctx)
	}
	return st, msg, verr
}

// recvMsg carries one stream.Recv result across the reader goroutine.
type recvMsg struct {
	resp *runnyv1.GetStatusResponse
	err  error
}

// snapshotReader pumps a WatchStatus stream into a channel so the follow loop
// can select on it against timers without blocking on Recv.
type snapshotReader struct {
	msgs chan recvMsg
}

func startReader(ctx context.Context, recv func() (*runnyv1.GetStatusResponse, error)) *snapshotReader {
	r := &snapshotReader{msgs: make(chan recvMsg, 1)}
	go func() {
		for {
			resp, err := recv()
			select {
			case r.msgs <- recvMsg{resp, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return r
}

// next returns the first snapshot, bounded by within (the establish check).
func (r *snapshotReader) next(ctx context.Context, within time.Duration) (*runnyv1.GetStatusResponse, error) {
	t := time.NewTimer(within)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.C:
		return nil, fmt.Errorf("no status within %s", durString(within))
	case m := <-r.msgs:
		return m.resp, m.err
	}
}

// openStream opens a WatchStatus stream, starts its reader, and waits for the
// first snapshot bounded by within. The returned closeStream stops the reader
// goroutine; the first snapshot confirms the socket is actually serving.
func (c *ctl) openStream(ctx context.Context, within time.Duration) (*snapshotReader, func(), *runnyv1.GetStatusResponse, error) {
	sctx, cancel := context.WithCancel(ctx)
	stream, err := c.client.WatchStatus(sctx, &runnyv1.WatchStatusRequest{})
	if err != nil {
		cancel()
		return nil, nil, nil, err
	}
	r := startReader(sctx, stream.Recv)
	first, err := r.next(ctx, within)
	if err != nil {
		cancel()
		return nil, nil, nil, err
	}
	return r, cancel, first, nil
}

// streamDrain follows one stream's live drain against the shared follow state.
// It returns:
//   - nil: the stream ended (the caller probes before the respawn cap);
//   - errStalled: no drain progress past the stall window with nothing
//     long-running or held to explain it;
//   - ctx err: cancelled.
//
// It NEVER reports a successor. A snapshot arriving on an established stream is
// always from the single process that stream is pinned to — never the respawn,
// which can surface only as a freshly-opened stream's first snapshot (the reopen
// path) or a probe/respawn poll. So a non-matching snapshot here is a predecessor
// — e.g. one the reader prefetched from the old process before the reload was
// accepted, in the launchd-restart race — and is discarded, never mistaken for
// the respawn.
//
// The stall timer lives in fs and is NOT re-armed on entry — it carries across
// reopens, so a flapping stream cannot reset the progress bound. A ctx
// cancellation is caught by the caller's own ctx.Err() check.
func (c *ctl) streamDrain(ctx context.Context, reader *snapshotReader, base baseline, opts followOpts, fs *followState) error {
	redraw := time.NewTicker(opts.redrawInterval)
	defer redraw.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-fs.stall.C:
			// The stall means "hung" only when nothing legitimately indefinite
			// explains the silence. Suppress and re-arm when: any slot is still
			// working an active state (every cycle state carries its own per-state
			// deadline — PROVISION alone is 180s, twice this window — so a frozen
			// drain_seq while a slot works is that deadline's business to enforce,
			// not a hang for the client to call); the exit gate is held
			// (operator-actionable); or the daemon predates drain_seq (protocol < 2,
			// no progress signal, so the stall would degrade into a wall-clock cap
			// on a drain — the cap this design refuses). What's left is a quiescent
			// fleet (all slots in BACKOFF) that still isn't exiting. (The timer
			// fired and the select drained fs.stall.C, so a bare Reset is safe.)
			if fs.last != nil && (fs.last.GetProtocolVersion() < 2 ||
				anySlotActive(fs.last) ||
				fs.last.GetExitHeld()) {
				fs.stall.Reset(opts.stallWindow)
				continue
			}
			return errStalled
		case <-redraw.C:
			if fs.last != nil {
				c.renderFollow(fs.last, fs.start)
			}
		case m := <-reader.msgs:
			if m.err != nil {
				return nil // stream ended; the caller probes
			}
			// Fold this snapshot only when it is the process the reload baselined.
			// A non-match is a predecessor the reader prefetched before acceptance
			// (the launchd-restart race); discard it rather than absorb it as the
			// respawn — the genuine successor arrives via a reopen or a probe.
			// Fold this snapshot only when it is the process the reload baselined.
			// A non-match is a predecessor the reader prefetched before acceptance
			// (the launchd-restart race); discard it rather than absorb it as the
			// respawn — the genuine successor arrives via a reopen or a probe.
			if !base.isSuccessor(m.resp) {
				c.fold(m.resp, fs, opts)
			}
		}
	}
}

// probeKind classifies a status probe of a dropped follow stream.
type probeKind int

const (
	probeStillOld   probeKind = iota // the baselined process answered: a transient drop
	probeNewProcess                  // a different process answered: the respawn already happened
	probeGone                        // no answer within the probe window: the daemon has exited
)

type probeResult struct {
	kind probeKind
	st   *runnyv1.GetStatusResponse // set for probeStillOld / probeNewProcess
}

// probeDaemon disambiguates a dropped follow stream: a transient blip while the
// old daemon still drains, or the daemon exiting for the respawn. A single
// failed GetStatus is not proof of exit, so it retries across the establish
// window; only sustained failure is probeGone.
func (c *ctl) probeDaemon(ctx context.Context, base baseline, opts followOpts) probeResult {
	deadline := time.Now().Add(opts.establishTimeout)
	for {
		pctx, cancel := context.WithTimeout(ctx, probeCallTimeout)
		st, err := c.client.GetStatus(pctx, &runnyv1.GetStatusRequest{})
		cancel()
		if err == nil {
			if base.isSuccessor(st) {
				return probeResult{kind: probeNewProcess, st: st}
			}
			return probeResult{kind: probeStillOld, st: st}
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return probeResult{kind: probeGone}
		}
		select {
		case <-ctx.Done():
			return probeResult{kind: probeGone}
		case <-time.After(opts.pollInterval):
		}
	}
}

// awaitRespawn polls until a new process answers (a changed boot_id), then
// returns its status and the verdict. The respawn cap (Clock B) starts here,
// after the daemon is confirmed gone, so a long healthy drain never trips it.
// The gRPC connection transparently reconnects to the restarted socket.
func (c *ctl) awaitRespawn(ctx context.Context, base baseline, wantSHA string, jobInFlight bool, opts followOpts) (*runnyv1.GetStatusResponse, string, error) {
	rctx, cancel := context.WithTimeout(ctx, opts.respawnTimeout)
	defer cancel()
	if !c.json {
		fmt.Fprintln(os.Stderr, "daemon exited; waiting for the respawn…")
	}
	tick := time.NewTicker(opts.pollInterval)
	defer tick.Stop()
	for {
		select {
		case <-rctx.Done():
			if ctx.Err() != nil {
				return nil, "", ctx.Err() // interrupt or overall timeout; caller interprets
			}
			return nil, "", fmt.Errorf("drain completed but no respawn observed within %s; the daemon may have crashed or launchd did not restart it — check `runnyctl status` and launchd.err.log",
				durString(opts.respawnTimeout))
		case <-tick.C:
			st, err := c.client.GetStatus(rctx, &runnyv1.GetStatusRequest{})
			if err != nil {
				continue // socket not back yet
			}
			if !base.isSuccessor(st) {
				continue // the old process is still answering; wait for the new one
			}
			msg, verr := respawnVerdict(st, wantSHA, jobInFlight)
			return st, msg, verr
		}
	}
}

// respawnVerdict classifies a respawned daemon's status against the config the
// reload validated. Pure, so the whole taxonomy is unit-testable. A non-nil
// error marks config drift — the respawn loaded a file other than the validated
// one — which the operator must act on; everything else is success, with a note
// when a job was still in flight as the predecessor went down.
func respawnVerdict(st *runnyv1.GetStatusResponse, wantSHA string, jobInFlight bool) (string, error) {
	got := st.GetConfigSha256()
	note := ""
	if d := st.GetDraining(); d != "" {
		note = " (the new daemon is already draining again: " + d + ")"
	}
	switch {
	case st.GetProtocolVersion() < 2 || got == "":
		return "daemon respawned, but it does not report its running config hash (protocol < 2); cannot verify it came up on " +
			shortHex(wantSHA) + " — upgrade the daemon to confirm" + note, nil
	case got != wantSHA:
		return "", fmt.Errorf(
			"daemon respawned on config %s, NOT the config you reloaded (%s); the on-disk file changed during the drain",
			shortHex(got), shortHex(wantSHA),
		)
	case jobInFlight:
		return "reloaded: respawned on config " + shortHex(wantSHA) +
			", but the previous daemon went down with a job still running — it may have been interrupted" + note, nil
	default:
		return "reloaded: respawned on config " + shortHex(wantSHA) + note, nil
	}
}

// anySlotInState reports whether any slot is in one of the given states.
func anySlotInState(resp *runnyv1.GetStatusResponse, states ...runnyv1.SlotState) bool {
	for _, s := range resp.GetSlots() {
		for _, want := range states {
			if s.GetState() == want {
				return true
			}
		}
	}
	return false
}

// anySlotActive reports whether any slot is still working through its cycle — any
// state other than BACKOFF, the paused/backing-off state a drain converges to.
// Every active state is bounded daemon-side, by its own per-state deadline (CLONE,
// BOOT, AWAIT_*, MINT_JIT, PROVISION, TEARDOWN, SECURE_SSH), by a duration budget
// (a long JOB), or by a progress watcher (an ENSURE_IMAGE pull), so a frozen
// drain_seq while a slot is active is that bound's business to enforce — not a
// hang for the client stall to call. The stall is left to fire only once the
// fleet is quiescent (all slots in BACKOFF) yet still not exiting.
//
// A WEDGED slot is excepted: it is a converged drain state (the daemon counts it
// as stable — it cannot start a job) even though it still reports its underlying
// state (e.g. TEARDOWN). Treating it as active would suppress the stall forever
// on a fleet that converged but never exits — exactly the hang the stall catches.
func anySlotActive(resp *runnyv1.GetStatusResponse) bool {
	for _, s := range resp.GetSlots() {
		if s.GetWedged() {
			continue
		}
		switch s.GetState() {
		case runnyv1.SlotState_SLOT_STATE_UNSPECIFIED, runnyv1.SlotState_SLOT_STATE_BACKOFF:
			// not active
		default:
			return true
		}
	}
	return false
}

// resetTimer safely re-arms t to d, draining a fired-but-unread tick first.
func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

// emitReloadWait writes the whole -wait result as ONE JSON document: the
// accepted ReloadResponse plus the respawn status when one was observed.
// respawn is omitted when no successor was seen (a Ctrl-C, a stall, or a
// respawn timeout).
func (c *ctl) emitReloadWait(accepted *runnyv1.ReloadResponse, respawn *runnyv1.GetStatusResponse) error {
	mo := protojson.MarshalOptions{Multiline: true, Indent: "  "}
	acceptedJSON, err := mo.Marshal(accepted)
	if err != nil {
		return err
	}
	wrapper := struct {
		Accepted json.RawMessage `json:"accepted"`
		Respawn  json.RawMessage `json:"respawn,omitempty"`
	}{Accepted: acceptedJSON}
	if respawn != nil {
		respawnJSON, err := mo.Marshal(respawn)
		if err != nil {
			return err
		}
		wrapper.Respawn = respawnJSON
	}
	b, err := json.MarshalIndent(wrapper, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(c.out, string(b))
	return err
}

// cancelled maps a cancelled follow to the right message and exit: an overall
// timeout is a failure to converge; a Ctrl-C is the operator choosing to stop
// watching, which does NOT cancel the drain.
func (c *ctl) cancelled(ctx context.Context) error {
	if errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
		return fmt.Errorf("gave up following the reload after the overall timeout; the drain continues on the daemon — watch `runnyctl status`")
	}
	fmt.Fprintln(os.Stderr, "\nstopped following; the drain continues on the daemon (this did not cancel it) — watch `runnyctl status`")
	return nil
}

func (c *ctl) renderFollow(resp *runnyv1.GetStatusResponse, start time.Time) {
	if c.json {
		return // JSON mode follows silently; the final status is emitted at the end
	}
	fmt.Fprint(c.out, "\x1b[2J\x1b[H") // clear; it's a live view
	fmt.Fprintf(c.out, "following reload — elapsed %s — Ctrl-C to stop following (the drain continues)\n\n",
		durString(time.Since(start)))
	c.renderStatus(resp)
	if resp.GetExitHeld() {
		fmt.Fprintln(c.out, "\nthe respawn is HELD: the on-disk config no longer validates — fix ~/.runny/config.yaml and the daemon will exit and respawn")
	}
}
