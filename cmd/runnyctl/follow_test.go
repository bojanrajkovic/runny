package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	runnyv1 "github.com/bojanrajkovic/runny/proto/runny/v1"
)

const (
	wantSHA  = "4a5b6c7d8e9f00112233445566778899aabbccddeeff00112233445566778899"
	otherSHA = "ffffffffffff00112233445566778899aabbccddeeff00112233445566778899"
)

// respawnVerdict is the whole outcome taxonomy in one pure function; the table
// pins each branch. The contract: a NON-NIL error means config drift (the
// operator must act); everything else is success or a warn.
func TestRespawnVerdict(t *testing.T) {
	status := func(proto uint32, got string) *runnyv1.GetStatusResponse {
		return &runnyv1.GetStatusResponse{ProtocolVersion: proto, ConfigSha256: got}
	}
	cases := []struct {
		name        string
		st          *runnyv1.GetStatusResponse
		jobInFlight bool
		wantErr     bool
		substr      string
	}{
		{
			name:   "proto<2 cannot verify the config — warn, not failure",
			st:     status(1, ""),
			substr: "does not report its running config hash",
		},
		{
			name:    "config drift — the respawn loaded a different file",
			st:      status(2, otherSHA),
			wantErr: true,
			substr:  "NOT the config you reloaded",
		},
		{
			name:   "respawned on the validated config, clean",
			st:     status(2, wantSHA),
			substr: "reloaded: respawned on config",
		},
		{
			name:        "respawned on the validated config but a job was in flight",
			st:          status(2, wantSHA),
			jobInFlight: true,
			substr:      "went down with a job still running",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, err := respawnVerdict(tc.st, wantSHA, tc.jobInFlight)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want a drift error, got msg=%q err=nil", msg)
				}
				if !strings.Contains(err.Error(), tc.substr) {
					t.Errorf("error = %q, want it to contain %q", err.Error(), tc.substr)
				}
				return
			}
			if err != nil {
				t.Fatalf("want success/warn, got err=%v", err)
			}
			if !strings.Contains(msg, tc.substr) {
				t.Errorf("verdict = %q, want it to contain %q", msg, tc.substr)
			}
		})
	}
}

// A new daemon already draining again must carry that into the verdict so the
// operator isn't surprised that a fresh reload is needed.
func TestRespawnVerdictCarriesReDrain(t *testing.T) {
	msg, err := respawnVerdict(&runnyv1.GetStatusResponse{
		ProtocolVersion: 2, ConfigSha256: wantSHA, Draining: "wedged guest",
	}, wantSHA, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(msg, "already draining again: wedged guest") {
		t.Errorf("verdict did not surface the re-drain: %q", msg)
	}
}

// boot_id is the positive discriminator; daemon_started is only the pre-2
// fallback. A nil timestamp must not read as a respawn.
func TestBaselineIsSuccessor(t *testing.T) {
	t0 := time.Now().Add(-time.Hour)
	mk := func(boot string, started *time.Time) *runnyv1.GetStatusResponse {
		r := &runnyv1.GetStatusResponse{BootId: boot}
		if started != nil {
			r.DaemonStarted = timestamppb.New(*started)
		}
		return r
	}
	t1 := time.Now()
	cases := []struct {
		name string
		base baseline
		st   *runnyv1.GetStatusResponse
		want bool
	}{
		{"boot_id flip = successor", baseline{bootID: "A"}, mk("B", nil), true},
		{"same boot_id = same process", baseline{bootID: "A"}, mk("A", nil), false},
		{"empty boot_id + no start = not yet proven", baseline{bootID: "A"}, mk("", nil), false},
		{"downgrade: empty boot_id + changed start = successor", baseline{bootID: "A", started: t0}, mk("", &t1), true},
		{"pre-2 fallback: changed start = successor", baseline{started: t0}, mk("", &t1), true},
		{"pre-2 fallback: same start = same process", baseline{started: t0}, mk("", &t0), false},
		{"pre-2 fallback: unset start does not prove a respawn", baseline{started: t0}, mk("", nil), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.base.isSuccessor(tc.st); got != tc.want {
				t.Errorf("isSuccessor = %v, want %v", got, tc.want)
			}
		})
	}
}

// scriptedStream replays queued (resp, err) from Recv, then blocks on `block`
// (closed in cleanup) so a follow can be left hanging to exercise the stall
// path without leaking the reader goroutine.
type scriptedStream struct {
	runnyv1.RunnyService_WatchStatusClient
	results []recvResult
	i       int
	block   chan struct{}
}

type recvResult struct {
	resp *runnyv1.GetStatusResponse
	err  error
}

func (s *scriptedStream) Recv() (*runnyv1.GetStatusResponse, error) {
	if s.i < len(s.results) {
		r := s.results[s.i]
		s.i++
		return r.resp, r.err
	}
	if s.block != nil {
		<-s.block
	}
	return nil, io.EOF
}

type fakeFollowClient struct {
	runnyv1.RunnyServiceClient
	stream   runnyv1.RunnyService_WatchStatusClient
	statusFn func() (*runnyv1.GetStatusResponse, error)
}

func (f *fakeFollowClient) WatchStatus(_ context.Context, _ *runnyv1.WatchStatusRequest, _ ...grpc.CallOption) (runnyv1.RunnyService_WatchStatusClient, error) {
	return f.stream, nil
}

func (f *fakeFollowClient) GetStatus(_ context.Context, _ *runnyv1.GetStatusRequest, _ ...grpc.CallOption) (*runnyv1.GetStatusResponse, error) {
	return f.statusFn()
}

// hangingStream returns a scriptedStream that replays results then blocks
// (rather than EOF-ing), with a cleanup that releases the reader goroutine.
func hangingStream(t *testing.T, results ...recvResult) *scriptedStream {
	s := &scriptedStream{results: results, block: make(chan struct{})}
	t.Cleanup(func() { close(s.block) })
	return s
}

func idleSnap(seq uint64) *runnyv1.GetStatusResponse {
	return &runnyv1.GetStatusResponse{
		BootId:          "A",
		ProtocolVersion: 2, // drain_seq is a protocol-2 signal; the stall is too
		DrainSeq:        seq,
		Draining:        "config reload (rpc)",
		Slots: []*runnyv1.SlotStatus{
			{Slot: "mac-1", State: runnyv1.SlotState_SLOT_STATE_BACKOFF, Paused: true},
		},
	}
}

func slotSnap(seq uint64, state runnyv1.SlotState) *runnyv1.GetStatusResponse {
	s := idleSnap(seq)
	s.Slots[0].State = state
	s.Slots[0].Paused = false
	return s
}

func testFollowOpts() followOpts {
	o := defaultFollowOpts(time.Second, 0)
	o.stallWindow = 40 * time.Millisecond
	o.establishTimeout = 200 * time.Millisecond
	o.redrawInterval = 10 * time.Millisecond
	o.pollInterval = 5 * time.Millisecond
	return o
}

func newFollowState(opts followOpts) *followState {
	return &followState{stall: time.NewTimer(opts.stallWindow), start: time.Now()}
}

// A converged, idle, no-progress stream with nothing long-running must trip the
// stall — the wedged-but-serving case the bound exists to catch.
func TestStreamDrainStallFiresWhenIdle(t *testing.T) {
	ctx := t.Context()
	reader := startReader(ctx, hangingStream(t, recvResult{resp: idleSnap(1)}).Recv)
	c := &ctl{client: &fakeFollowClient{}, out: &bytes.Buffer{}}
	opts := testFollowOpts()
	err := c.streamDrain(ctx, reader, baseline{bootID: "A"}, opts, newFollowState(opts))
	if err != errStalled {
		t.Fatalf("err = %v, want errStalled", err)
	}
}

// A WEDGED slot is a converged drain state even though it still reports an
// underlying state like TEARDOWN; it must NOT suppress the stall, or a fleet that
// converged (every slot wedged or paused) but never exits hangs the follow
// forever — the very case the stall now exists to catch.
func TestStreamDrainStallFiresOnWedgedSlot(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	snap := idleSnap(1)
	snap.Slots[0].State = runnyv1.SlotState_SLOT_STATE_TEARDOWN
	snap.Slots[0].Paused = false
	snap.Slots[0].Wedged = true
	reader := startReader(ctx, hangingStream(t, recvResult{resp: snap}).Recv)
	c := &ctl{client: &fakeFollowClient{}, out: &bytes.Buffer{}}
	opts := testFollowOpts()
	done := make(chan error, 1)
	go func() { done <- c.streamDrain(ctx, reader, baseline{bootID: "A"}, opts, newFollowState(opts)) }()
	select {
	case err := <-done:
		if err != errStalled {
			t.Fatalf("err = %v, want errStalled (a wedged slot is converged, not active)", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("streamDrain did not stall — the wedged slot was wrongly treated as active")
	}
}

// A pre-2 daemon pins drain_seq at 0 — it has no progress signal — so the stall
// must NOT fire against it, or it degrades into a wall-clock cap on a drain that
// can validly run as long as any bounded state allows. streamDrain must keep
// following until cancel.
func TestStreamDrainStallDisabledForPreV2(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	snap := idleSnap(1)
	snap.ProtocolVersion = 1 // supports Reload, predates the drain_seq signal
	reader := startReader(ctx, hangingStream(t, recvResult{resp: snap}).Recv)
	c := &ctl{client: &fakeFollowClient{}, out: &bytes.Buffer{}}
	opts := testFollowOpts()
	done := make(chan error, 1)
	go func() {
		err := c.streamDrain(ctx, reader, baseline{bootID: "A"}, opts, newFollowState(opts))
		done <- err
	}()
	// Several stall windows must pass without errStalled.
	select {
	case err := <-done:
		t.Fatalf("streamDrain returned %v; a pre-2 daemon has no progress signal and must not trip the stall", err)
	case <-time.After(300 * time.Millisecond):
	}
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("after cancel err = %v, want context.Canceled", err)
	}
}

// The stall must be SUPPRESSED while any slot is still working an active state —
// every cycle state is bounded daemon-side by its own per-state deadline (e.g.
// PROVISION at 180s, twice the stall window), a duration budget (JOB), or a pull
// watcher (ENSURE_IMAGE) — or while the exit gate is held. None is a hang.
// streamDrain must keep following until cancel.
func TestStreamDrainStallSuppressed(t *testing.T) {
	for _, tc := range []struct {
		name string
		snap *runnyv1.GetStatusResponse
	}{
		{"running job", slotSnap(1, runnyv1.SlotState_SLOT_STATE_JOB)},
		{"image pull", slotSnap(1, runnyv1.SlotState_SLOT_STATE_ENSURE_IMAGE)},
		{"provisioning (180s daemon deadline)", slotSnap(1, runnyv1.SlotState_SLOT_STATE_PROVISION)},
		{"booting", slotSnap(1, runnyv1.SlotState_SLOT_STATE_BOOT)},
		{"awaiting ssh", slotSnap(1, runnyv1.SlotState_SLOT_STATE_AWAIT_SSH)},
		{"held exit gate", func() *runnyv1.GetStatusResponse { s := idleSnap(1); s.ExitHeld = true; return s }()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			reader := startReader(ctx, hangingStream(t, recvResult{resp: tc.snap}).Recv)
			c := &ctl{client: &fakeFollowClient{}, out: &bytes.Buffer{}}
			opts := testFollowOpts()
			done := make(chan error, 1)
			go func() {
				err := c.streamDrain(ctx, reader, baseline{bootID: "A"}, opts, newFollowState(opts))
				done <- err
			}()
			// Several stall windows must pass without errStalled while suppressed.
			select {
			case err := <-done:
				t.Fatalf("streamDrain returned %v while it should still be following", err)
			case <-time.After(300 * time.Millisecond):
			}
			cancel()
			if err := <-done; err != context.Canceled {
				t.Fatalf("after cancel err = %v, want context.Canceled", err)
			}
		})
	}
}

// A foreign boot_id arriving mid-stream is NOT a successor: a stream is pinned to
// one process, so this is a predecessor the reader prefetched before acceptance
// (the launchd-restart race), not the respawn. streamDrain must discard it — not
// fold it, not report it — and let the stream end carry the wait to the probe.
func TestStreamDrainDiscardsForeignMidStreamSnapshot(t *testing.T) {
	ctx := t.Context()
	// baseline is the accepting process B; this A on a different config would, if
	// mistaken for the respawn, report a false drift.
	predecessor := &runnyv1.GetStatusResponse{BootId: "A", ProtocolVersion: 2, ConfigSha256: otherSHA}
	stream := &scriptedStream{results: []recvResult{
		{resp: predecessor},
		{err: io.EOF},
	}}
	reader := startReader(ctx, stream.Recv)
	c := &ctl{client: &fakeFollowClient{}, out: &bytes.Buffer{}}
	opts := testFollowOpts()
	fs := newFollowState(opts)
	if err := c.streamDrain(ctx, reader, baseline{bootID: "B"}, opts, fs); err != nil {
		t.Fatalf("err = %v, want nil (the predecessor is discarded, then the stream ends)", err)
	}
	if fs.last != nil {
		t.Errorf("predecessor folded into follow state (fs.last=%+v); it must be discarded", fs.last)
	}
}

// A clean stream end means "the daemon exited for the respawn": streamDrain
// returns nil so the caller advances to the probe/respawn phase.
func TestStreamDrainEndReturnsNil(t *testing.T) {
	ctx := t.Context()
	stream := &scriptedStream{results: []recvResult{
		{resp: idleSnap(1)},
		{err: io.EOF},
	}}
	reader := startReader(ctx, stream.Recv)
	c := &ctl{client: &fakeFollowClient{}, out: &bytes.Buffer{}}
	opts := testFollowOpts()
	if err := c.streamDrain(ctx, reader, baseline{bootID: "A"}, opts, newFollowState(opts)); err != nil {
		t.Fatalf("stream end: got err=%v, want nil", err)
	}
}

// fs.jobInFlight reflects the last old-process snapshot: a JOB slot present as
// the daemon goes down colors the success verdict.
func TestStreamDrainReportsJobInFlight(t *testing.T) {
	ctx := t.Context()
	stream := &scriptedStream{results: []recvResult{
		{resp: slotSnap(1, runnyv1.SlotState_SLOT_STATE_JOB)},
		{err: io.EOF},
	}}
	reader := startReader(ctx, stream.Recv)
	c := &ctl{client: &fakeFollowClient{}, out: &bytes.Buffer{}}
	opts := testFollowOpts()
	fs := newFollowState(opts)
	err := c.streamDrain(ctx, reader, baseline{bootID: "A"}, opts, fs)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !fs.jobInFlight {
		t.Error("jobInFlight = false, want true when a JOB slot was present at stream end")
	}
}

// flappyClient hands out a FRESH stream on every WatchStatus — each yielding one
// idle snapshot then EOF — and answers GetStatus with the same old process. It
// models a daemon that flaps its stream just under the stall window while the
// drain makes no progress.
type flappyClient struct {
	runnyv1.RunnyServiceClient
	snap *runnyv1.GetStatusResponse
}

func (f *flappyClient) WatchStatus(_ context.Context, _ *runnyv1.WatchStatusRequest, _ ...grpc.CallOption) (runnyv1.RunnyService_WatchStatusClient, error) {
	return &scriptedStream{results: []recvResult{{resp: f.snap}, {err: io.EOF}}}, nil
}

func (f *flappyClient) GetStatus(_ context.Context, _ *runnyv1.GetStatusRequest, _ ...grpc.CallOption) (*runnyv1.GetStatusResponse, error) {
	return f.snap, nil
}

// reopenFailClient models a daemon that is alive on the unary path (GetStatus
// answers) but whose WatchStatus reopen keeps failing — a transient stream-open
// error during a healthy drain.
type reopenFailClient struct {
	runnyv1.RunnyServiceClient
	statusFn func() (*runnyv1.GetStatusResponse, error)
}

func (f *reopenFailClient) WatchStatus(_ context.Context, _ *runnyv1.WatchStatusRequest, _ ...grpc.CallOption) (runnyv1.RunnyService_WatchStatusClient, error) {
	return nil, errors.New("transient stream-open failure")
}

func (f *reopenFailClient) GetStatus(_ context.Context, _ *runnyv1.GetStatusRequest, _ ...grpc.CallOption) (*runnyv1.GetStatusResponse, error) {
	return f.statusFn()
}

// The headline silent-hang: a daemon that flaps its stream (one snapshot then
// EOF, repeatedly) while drain_seq stays frozen must NOT reset the stall on each
// reopen. follow must still declare errStalled rather than spin forever.
func TestFollowFlappingStreamStillStalls(t *testing.T) {
	ctx := t.Context()
	snap := idleSnap(1) // boot_id "A", frozen drain_seq, no JOB
	c := &ctl{client: &flappyClient{snap: snap}, out: &bytes.Buffer{}}
	opts := testFollowOpts()
	reader, closeStream, first, err := c.openStream(ctx, opts.establishTimeout)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, verr := c.follow(ctx, reader, closeStream, first, baseline{bootID: "A"}, wantSHA, opts)
		done <- verr
	}()
	select {
	case err := <-done:
		if !errors.Is(err, errStalled) {
			t.Fatalf("follow returned %v, want errStalled (the flapping stream must not reset the stall forever)", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("follow spun forever on a flapping stream instead of stalling")
	}
}

// The establish snapshot is captured BEFORE the reload is accepted, so a boot_id
// that differs from the accepting baseline is a PREDECESSOR — a launchd restart
// that raced the reload (boot_id A on the wire, boot_id B accepting), never the
// respawn. follow must discard it and keep waiting for the genuine successor,
// not resolve a verdict against the predecessor's config (which here is a
// different SHA, so a mistaken verdict would falsely read as config drift).
func TestFollowDiscardsPreAcceptPredecessor(t *testing.T) {
	ctx := t.Context()
	// first: predecessor A, on a DIFFERENT config than the reload validated.
	first := &runnyv1.GetStatusResponse{BootId: "A", ProtocolVersion: 2, ConfigSha256: otherSHA}
	// The establish stream (opened against A) drops; the probe then finds the
	// genuine respawn C, up on the validated config.
	reader := startReader(ctx, (&scriptedStream{results: []recvResult{{err: io.EOF}}}).Recv)
	respawn := &runnyv1.GetStatusResponse{BootId: "C", ProtocolVersion: 2, ConfigSha256: wantSHA}
	fc := &fakeFollowClient{statusFn: func() (*runnyv1.GetStatusResponse, error) { return respawn, nil }}
	c := &ctl{client: fc, out: &bytes.Buffer{}}
	st, msg, err := c.follow(ctx, reader, func() {}, first, baseline{bootID: "B"}, wantSHA, testFollowOpts())
	if err != nil {
		t.Fatalf("verdict error — the pre-accept predecessor was mistaken for the respawn: %v", err)
	}
	if st.GetBootId() != "C" {
		t.Fatalf("verdict resolved against boot_id %q, want the real respawn C", st.GetBootId())
	}
	if !strings.Contains(msg, "respawned on config") {
		t.Errorf("msg = %q, want a clean respawn verdict", msg)
	}
}

// Beyond the establish snapshot: after discarding `first` as a predecessor, a
// SECOND predecessor snapshot the reader prefetched from the same pre-accept
// stream (a heartbeat/transition queued while the Reload RPC ran) must also be
// discarded, not mistaken for the respawn. The genuine successor C is found only
// after that stream ends and a probe reaches the real process.
func TestFollowDiscardsPrefetchedPredecessor(t *testing.T) {
	ctx := t.Context()
	first := &runnyv1.GetStatusResponse{BootId: "A", ProtocolVersion: 2, ConfigSha256: otherSHA}
	prefetched := &runnyv1.GetStatusResponse{BootId: "A", ProtocolVersion: 2, ConfigSha256: otherSHA}
	// The pre-accept stream (opened against A) delivers the prefetched predecessor
	// snapshot, then ends; the probe then finds the genuine respawn C.
	reader := startReader(ctx, (&scriptedStream{results: []recvResult{
		{resp: prefetched}, {err: io.EOF},
	}}).Recv)
	respawn := &runnyv1.GetStatusResponse{BootId: "C", ProtocolVersion: 2, ConfigSha256: wantSHA}
	fc := &fakeFollowClient{statusFn: func() (*runnyv1.GetStatusResponse, error) { return respawn, nil }}
	c := &ctl{client: fc, out: &bytes.Buffer{}}
	st, msg, err := c.follow(ctx, reader, func() {}, first, baseline{bootID: "B"}, wantSHA, testFollowOpts())
	if err != nil {
		t.Fatalf("verdict error — a prefetched predecessor was mistaken for the respawn: %v", err)
	}
	if st.GetBootId() != "C" {
		t.Fatalf("verdict resolved against boot_id %q, want the real respawn C", st.GetBootId())
	}
	if !strings.Contains(msg, "respawned on config") {
		t.Errorf("msg = %q, want a clean respawn verdict", msg)
	}
}

// A transient stream-reopen failure while the daemon is still alive must NOT
// start the respawn cap — doing so would time out against a daemon that never
// exited and report a false "no respawn observed". follow must keep re-probing
// the live daemon (here: forever, until cancelled) rather than enter the cap.
func TestFollowReopenFailureDoesNotStartRespawnCap(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// The live stream drops immediately; the daemon then stays old on every probe
	// while WatchStatus (the reopen) keeps failing.
	reader := startReader(ctx, (&scriptedStream{results: []recvResult{{err: io.EOF}}}).Recv)
	fc := &reopenFailClient{statusFn: func() (*runnyv1.GetStatusResponse, error) {
		return &runnyv1.GetStatusResponse{BootId: "A", ProtocolVersion: 2}, nil
	}}
	c := &ctl{client: fc, out: &bytes.Buffer{}}
	opts := testFollowOpts()
	opts.respawnTimeout = 30 * time.Millisecond // would fire fast if we wrongly entered the cap
	errc := make(chan error, 1)
	go func() {
		_, _, err := c.follow(ctx, reader, func() {}, idleSnap(1), baseline{bootID: "A"}, wantSHA, opts)
		errc <- err
	}()
	// Many respawn-cap windows must pass without follow returning a verdict/error.
	select {
	case err := <-errc:
		t.Fatalf("follow returned %v; a reopen failure must re-probe the live daemon, not start the respawn cap", err)
	case <-time.After(300 * time.Millisecond):
	}
	cancel()
	<-errc // let the goroutine unwind
}

// A single failed GetStatus is NOT proof of exit; probeDaemon declares gone only
// after the establish window of failures.
func TestProbeDaemonGoneOnlyAfterWindow(t *testing.T) {
	calls := 0
	fc := &fakeFollowClient{statusFn: func() (*runnyv1.GetStatusResponse, error) {
		calls++
		return nil, io.ErrUnexpectedEOF
	}}
	c := &ctl{client: fc, out: &bytes.Buffer{}}
	opts := testFollowOpts()
	p := c.probeDaemon(t.Context(), baseline{bootID: "A"}, opts)
	if p.kind != probeGone {
		t.Fatalf("kind = %v, want probeGone", p.kind)
	}
	if calls < 2 {
		t.Errorf("probed %d time(s); a single failure must not be declared gone", calls)
	}
}

func TestProbeDaemonStillOldVsNew(t *testing.T) {
	t.Run("still old", func(t *testing.T) {
		fc := &fakeFollowClient{statusFn: func() (*runnyv1.GetStatusResponse, error) {
			return &runnyv1.GetStatusResponse{BootId: "A"}, nil
		}}
		c := &ctl{client: fc, out: &bytes.Buffer{}}
		if p := c.probeDaemon(t.Context(), baseline{bootID: "A"}, testFollowOpts()); p.kind != probeStillOld {
			t.Fatalf("kind = %v, want probeStillOld", p.kind)
		}
	})
	t.Run("new process", func(t *testing.T) {
		fc := &fakeFollowClient{statusFn: func() (*runnyv1.GetStatusResponse, error) {
			return &runnyv1.GetStatusResponse{BootId: "B"}, nil
		}}
		c := &ctl{client: fc, out: &bytes.Buffer{}}
		if p := c.probeDaemon(t.Context(), baseline{bootID: "A"}, testFollowOpts()); p.kind != probeNewProcess {
			t.Fatalf("kind = %v, want probeNewProcess", p.kind)
		}
	})
}

// awaitRespawn returns the verdict once a NEW boot_id answers, and ignores the
// old process still answering in the meantime.
func TestAwaitRespawnDetectsNewProcess(t *testing.T) {
	calls := 0
	fc := &fakeFollowClient{statusFn: func() (*runnyv1.GetStatusResponse, error) {
		calls++
		if calls < 2 {
			return &runnyv1.GetStatusResponse{BootId: "A"}, nil // old process still up
		}
		return &runnyv1.GetStatusResponse{BootId: "B", ProtocolVersion: 2, ConfigSha256: wantSHA}, nil
	}}
	c := &ctl{client: fc, out: &bytes.Buffer{}}
	st, msg, err := c.awaitRespawn(t.Context(), baseline{bootID: "A"}, wantSHA, false, testFollowOpts())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if st.GetBootId() != "B" || !strings.Contains(msg, "respawned on config") {
		t.Errorf("got st=%+v msg=%q", st, msg)
	}
}

func TestAwaitRespawnTimesOut(t *testing.T) {
	fc := &fakeFollowClient{statusFn: func() (*runnyv1.GetStatusResponse, error) {
		return &runnyv1.GetStatusResponse{BootId: "A"}, nil // old process never leaves
	}}
	c := &ctl{client: fc, out: &bytes.Buffer{}}
	opts := testFollowOpts()
	opts.respawnTimeout = 30 * time.Millisecond
	_, _, err := c.awaitRespawn(t.Context(), baseline{bootID: "A"}, wantSHA, false, opts)
	if err == nil || !strings.Contains(err.Error(), "no respawn observed") {
		t.Fatalf("err = %v, want a respawn-timeout error", err)
	}
}

// The -wait JSON output is ONE document: the accepted ReloadResponse plus the
// respawn status when one was seen, omitted otherwise.
func TestEmitReloadWaitSingleDocument(t *testing.T) {
	accepted := &runnyv1.ReloadResponse{Accepted: true, ConfigSha256: wantSHA, AcceptingBootId: "A"}
	respawn := &runnyv1.GetStatusResponse{ProtocolVersion: 2, ConfigSha256: wantSHA, BootId: "B"}
	t.Run("with respawn", func(t *testing.T) {
		var buf bytes.Buffer
		c := &ctl{client: &fakeFollowClient{}, json: true, out: &buf}
		if err := c.emitReloadWait(accepted, respawn); err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Accepted json.RawMessage `json:"accepted"`
			Respawn  json.RawMessage `json:"respawn"`
		}
		if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
			t.Fatalf("output is not one JSON document: %v\n%s", err, buf.String())
		}
		if len(doc.Accepted) == 0 || len(doc.Respawn) == 0 {
			t.Errorf("missing sub-object: accepted=%s respawn=%s", doc.Accepted, doc.Respawn)
		}
	})
	t.Run("without respawn omits the field", func(t *testing.T) {
		var buf bytes.Buffer
		c := &ctl{client: &fakeFollowClient{}, json: true, out: &buf}
		if err := c.emitReloadWait(accepted, nil); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(buf.String(), "respawn") {
			t.Errorf("respawn should be omitted when no successor was seen:\n%s", buf.String())
		}
	})
}
