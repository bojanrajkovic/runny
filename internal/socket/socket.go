// Package socket serves the runny.v1 control surface over the daemon's unix
// socket. runnyctl and RunnyBar are equal clients of this server (ADR-0006).
package socket

import (
	"context"
	"fmt"
	"net"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/bojanrajkovic/runny/internal/cycle"
	"github.com/bojanrajkovic/runny/internal/home"
	"github.com/bojanrajkovic/runny/internal/logring"
	"github.com/bojanrajkovic/runny/internal/statemachine"
	runnyv1 "github.com/bojanrajkovic/runny/proto/runny/v1"
)

// DoctorCheck is one validation result.
type DoctorCheck struct {
	Name   string
	OK     bool
	Detail string
}

// ReloadResult mirrors runny.v1.ReloadResponse: the verdict of a reload
// preflight plus the drain it did (or did not) start. The daemon owns the
// drain; this is only the synchronous answer.
type ReloadResult struct {
	Accepted            bool
	StartedDrain        bool
	FailedChecks        []DoctorCheck
	Warnings            []DoctorCheck
	Draining            string
	SlotCount           int
	OperatorPausedSlots []string
	ConfigSHA256        string
}

// Server implements runny.v1.RunnyService.
type Server struct {
	runnyv1.UnimplementedRunnyServiceServer

	Slots []*statemachine.Slot
	// Ring is the daemon's own log; RunnerRing holds guest runner output
	// lines (slot/cycle in attrs). StreamLogs serves RunnerRing by default.
	Ring       *logring.Ring
	RunnerRing *logring.Ring
	Stores     func(slot string) cycle.Store
	DoctorFn   func(ctx context.Context) []DoctorCheck
	Started    time.Time
	Version    string
	// ReloadFn validates the on-disk config and (on acceptance) starts the
	// drain toward a respawn. It is called unconditionally — even while a
	// drain is already active, the verdict matters because the respawn loads
	// the on-disk file regardless. Nil = Unimplemented (handler unwired).
	ReloadFn func(ctx context.Context, reason string) ReloadResult
	// DrainingFn reports the active drain reason ("" = not draining),
	// including the exit-gate hold annotation when held. Nil = never
	// draining.
	DrainingFn func() string
	// Config carries the limits InjectDebugKey validates against (hold cap,
	// the queue/service bounds for the synchronous wait).
	Config *home.Config

	// watch fan-out
	mu      sync.Mutex
	watchID int
	watches map[int]chan struct{}
}

// NewServer wires the slots' OnChange into the watch fan-out.
func NewServer(slots []*statemachine.Slot, ring, runnerRing *logring.Ring,
	stores func(string) cycle.Store, doctor func(context.Context) []DoctorCheck, version string,
	cfg *home.Config,
) *Server {
	s := &Server{
		Slots:      slots,
		Ring:       ring,
		RunnerRing: runnerRing,
		Stores:     stores,
		DoctorFn:   doctor,
		Started:    time.Now(),
		Version:    version,
		Config:     cfg,
		watches:    map[int]chan struct{}{},
	}
	for _, slot := range slots {
		slot.OnChange(func(statemachine.Status) { s.notify() })
	}
	return s
}

func (s *Server) notify() {
	s.mu.Lock()
	for _, ch := range s.watches {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	s.mu.Unlock()
}

// Serve listens on the unix socket (owner-only) until ctx ends.
func (s *Server) Serve(ctx context.Context, socketPath string) error {
	_ = os.Remove(socketPath) // stale socket from a previous run
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", socketPath, err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		return fmt.Errorf("restricting socket perms: %w", err)
	}
	g := grpc.NewServer()
	runnyv1.RegisterRunnyServiceServer(g, s)
	go func() {
		<-ctx.Done()
		// GracefulStop waits for in-flight RPCs — including the streaming
		// ones, which only end when the client hangs up. RunnyBar keeps a
		// WatchStatus stream open by design, so an unbounded graceful phase
		// means SIGTERM never terminates the daemon. Give unary RPCs a
		// moment, then cut the streams.
		done := make(chan struct{})
		go func() {
			g.GracefulStop()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			g.Stop()
		}
	}()
	if err := g.Serve(ln); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

// draining is the nil-tolerant read of DrainingFn.
func (s *Server) draining() string {
	if s.DrainingFn == nil {
		return ""
	}
	return s.DrainingFn()
}

func (s *Server) snapshot() *runnyv1.GetStatusResponse {
	resp := &runnyv1.GetStatusResponse{
		DaemonStarted: timestamppb.New(s.Started),
		Version:       s.Version,
		Draining:      s.draining(),
	}
	// The config-derived InjectDebugKey wait, so `runnyctl debug` can size its
	// client deadline to outlast the daemon (else a timeout lies — see #0).
	if s.Config != nil {
		_, handlerWait := s.injectBounds()
		resp.InjectHandlerWait = durationpb.New(handlerWait)
	}
	for _, slot := range s.Slots {
		resp.Slots = append(resp.Slots, statusToProto(slot.Status()))
	}
	return resp
}

// injectBounds derives the InjectDebugKey timing from config: queueBound is the
// FSM's worst-case dequeue latency (one in-flight reconcile), handlerWait the
// daemon's total synchronous wait (dequeue + per-command work + slack). Exposed
// on GetStatus so a client waits at least handlerWait — a shorter client
// deadline on a host with raised reconcile_interval/secure_ssh makes the daemon
// outlive the client and report "nothing injected" while the key installs.
func (s *Server) injectBounds() (queueBound, handlerWait time.Duration) {
	queueBound = s.Config.Limits.ReconcileInterval.D() + 5*time.Second
	serviceBound := 3*s.Config.Deadlines.SecureSSH.D() + 10*time.Second
	return queueBound, queueBound + serviceBound + 5*time.Second
}

func (s *Server) GetStatus(ctx context.Context, _ *runnyv1.GetStatusRequest) (*runnyv1.GetStatusResponse, error) {
	return s.snapshot(), nil
}

func (s *Server) WatchStatus(_ *runnyv1.WatchStatusRequest, stream grpc.ServerStreamingServer[runnyv1.GetStatusResponse]) error {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	id := s.watchID
	s.watchID++
	s.watches[id] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.watches, id)
		s.mu.Unlock()
	}()

	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	if err := stream.Send(s.snapshot()); err != nil {
		return err
	}
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case <-ch:
		case <-tick.C:
		}
		if err := stream.Send(s.snapshot()); err != nil {
			return err
		}
	}
}

func (s *Server) StreamLogs(req *runnyv1.StreamLogsRequest, stream grpc.ServerStreamingServer[runnyv1.LogLine]) error {
	ring := s.RunnerRing
	keep := func(logring.Entry) bool { return true }
	switch {
	case req.GetDaemon():
		if req.GetSlot() != "" {
			return status.Error(codes.InvalidArgument, "slot filter does not apply to the daemon log")
		}
		ring = s.Ring
	case req.GetSlot() != "":
		slot, err := s.findSlot(req.GetSlot())
		if err != nil {
			return status.Error(codes.NotFound, err.Error())
		}
		// Filter on the RESOLVED name: req may carry a runner name, which
		// would never match the slot attr the ring entries carry.
		want := slot.Name()
		keep = func(e logring.Entry) bool { return e.Attrs["slot"] == want }
	}
	// With a filter, replay counts matching lines: subscribe to the whole
	// buffer and tail the survivors, so `-replay 50 mac-1` means 50 lines
	// of mac-1, not mac-1's share of the last 50 global lines.
	replay := int(req.GetReplay())
	subscribeReplay := replay
	if req.GetSlot() != "" && replay > 0 {
		subscribeReplay = 1 << 20 // effectively "all of the ring"
	}
	if !req.GetFollow() {
		for _, e := range tailKept(ring.Snapshot(subscribeReplay), keep, replay) {
			if err := stream.Send(toLogLine(e)); err != nil {
				return err
			}
		}
		return nil
	}
	snap, ch, cancel := ring.Subscribe(subscribeReplay)
	defer cancel()
	for _, e := range tailKept(snap, keep, replay) {
		if err := stream.Send(toLogLine(e)); err != nil {
			return err
		}
	}
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case e := <-ch:
			if !keep(e) {
				continue
			}
			if err := stream.Send(toLogLine(e)); err != nil {
				return err
			}
		}
	}
}

// tailKept filters entries and returns the last n survivors.
func tailKept(entries []logring.Entry, keep func(logring.Entry) bool, n int) []logring.Entry {
	var out []logring.Entry
	for _, e := range entries {
		if keep(e) {
			out = append(out, e)
		}
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

func toLogLine(e logring.Entry) *runnyv1.LogLine {
	return &runnyv1.LogLine{
		Time:    timestamppb.New(e.Time),
		Level:   e.Level,
		Message: e.Message,
		Attrs:   e.Attrs,
	}
}

// cycleSuffixRE is the -<cycle8> tail of a runner name.
var cycleSuffixRE = regexp.MustCompile(`-[0-9a-f]{8}$`)

// findSlot resolves an operator-supplied handle: the bare slot name, the
// live cycle's runner name, or any runner name of the right shape
// (<prefix>-<slot>-<cycle8>, e.g. copied from the GitHub runners page after
// the cycle ended) — status displays runner names, so commands must accept
// what status shows. Pool names may contain dashes, so the structural match
// errors on ambiguity rather than guessing.
func (s *Server) findSlot(name string) (*statemachine.Slot, error) {
	for _, slot := range s.Slots {
		if slot.Name() == name || slot.Status().RunnerName == name {
			return slot, nil
		}
	}
	if base := cycleSuffixRE.ReplaceAllString(name, ""); base != name {
		var matches []*statemachine.Slot
		for _, slot := range s.Slots {
			if strings.HasSuffix(base, "-"+slot.Name()) {
				matches = append(matches, slot)
			}
		}
		switch len(matches) {
		case 1:
			return matches[0], nil
		case 0:
			// fall through to the not-found error
		default:
			names := make([]string, len(matches))
			for i, m := range matches {
				names[i] = m.Name()
			}
			return nil, fmt.Errorf("runner name %q is ambiguous between slots %v — use the slot name", name, names)
		}
	}
	return nil, fmt.Errorf("no slot matches %q (use the slot name, or a runner name as shown by status)", name)
}

func (s *Server) Recycle(ctx context.Context, req *runnyv1.RecycleRequest) (*runnyv1.RecycleResponse, error) {
	slot, err := s.findSlot(req.GetSlot())
	if err != nil {
		return nil, err
	}
	if !slot.Command(statemachine.Command{
		Kind: statemachine.CmdRecycle, Reason: req.GetReason(),
		CancelJob: req.GetCancelRunningJob(),
	}) {
		return nil, fmt.Errorf("slot %s is not accepting commands", req.GetSlot())
	}
	return &runnyv1.RecycleResponse{}, nil
}

func (s *Server) Pause(ctx context.Context, req *runnyv1.PauseRequest) (*runnyv1.PauseResponse, error) {
	slot, err := s.findSlot(req.GetSlot())
	if err != nil {
		return nil, err
	}
	// Mirror Recycle: a full command buffer (the drainer saturates
	// non-converged slots with re-issued pause+recycle pairs) must surface as
	// an error, never a silent drop reported as success — the
	// silent-failure-proofness invariant.
	if !slot.Command(statemachine.Command{Kind: statemachine.CmdPause}) {
		return nil, fmt.Errorf("slot %s is not accepting commands", req.GetSlot())
	}
	resp := &runnyv1.PauseResponse{}
	// Pause during a drain is allowed (idempotent; the drain wants slots
	// paused anyway) but the operator must learn it is in-memory: the
	// respawn at the drain's end silently clears it, a window that can last
	// hours (a running job finishes first).
	if d := s.draining(); d != "" {
		resp.Note = fmt.Sprintf("daemon is draining for restart (%s); pause is in-memory and will not survive the respawn", d)
	}
	return resp, nil
}

func (s *Server) Resume(ctx context.Context, req *runnyv1.ResumeRequest) (*runnyv1.ResumeResponse, error) {
	// A resume mid-drain would silently fight the drainer (which re-issues
	// pause until convergence); refuse with the cause instead.
	if d := s.draining(); d != "" {
		return nil, status.Errorf(codes.FailedPrecondition, "daemon is draining: %s; resume after the respawn", d)
	}
	slot, err := s.findSlot(req.GetSlot())
	if err != nil {
		return nil, err
	}
	slot.Command(statemachine.Command{Kind: statemachine.CmdResume})
	return &runnyv1.ResumeResponse{}, nil
}

func (s *Server) Reload(ctx context.Context, req *runnyv1.ReloadRequest) (*runnyv1.ReloadResponse, error) {
	if s.ReloadFn == nil {
		return nil, status.Error(codes.Unimplemented, "reload is not wired on this server")
	}
	// No draining gate: the preflight runs (and its verdict is reported)
	// even mid-drain — the imminent respawn loads the on-disk file whether
	// or not it was validated, so "refused because already draining" would
	// invert the operator's reading. The handler never blocks on
	// convergence; that is observed via status/watch.
	r := s.ReloadFn(ctx, req.GetReason())
	resp := &runnyv1.ReloadResponse{
		Accepted:            r.Accepted,
		StartedDrain:        r.StartedDrain,
		Draining:            r.Draining,
		SlotCount:           int32(r.SlotCount),
		OperatorPausedSlots: r.OperatorPausedSlots,
		ConfigSha256:        r.ConfigSHA256,
	}
	for _, c := range r.FailedChecks {
		resp.FailedChecks = append(resp.FailedChecks, &runnyv1.DoctorCheck{Name: c.Name, Ok: c.OK, Detail: c.Detail})
	}
	for _, c := range r.Warnings {
		resp.Warnings = append(resp.Warnings, &runnyv1.DoctorCheck{Name: c.Name, Ok: c.OK, Detail: c.Detail})
	}
	return resp, nil
}

func (s *Server) Why(ctx context.Context, req *runnyv1.WhyRequest) (*runnyv1.WhyResponse, error) {
	slot, err := s.findSlot(req.GetSlot())
	if err != nil {
		return nil, err
	}
	n := int(req.GetCycles())
	if n == 0 {
		n = 1
	}
	// The store is keyed by the RESOLVED slot name: req may carry a runner
	// name, which as a store key would silently read an empty directory.
	recs, err := s.Stores(slot.Name()).Recent(n, slot.Status().CycleID)
	if err != nil {
		return nil, err
	}
	resp := &runnyv1.WhyResponse{}
	for _, r := range recs {
		resp.Cycles = append(resp.Cycles, recordToProto(r))
	}
	return resp, nil
}

func (s *Server) Doctor(ctx context.Context, _ *runnyv1.DoctorRequest) (*runnyv1.DoctorResponse, error) {
	resp := &runnyv1.DoctorResponse{}
	for _, c := range s.DoctorFn(ctx) {
		resp.Checks = append(resp.Checks, &runnyv1.DoctorCheck{Name: c.Name, Ok: c.OK, Detail: c.Detail})
	}
	return resp, nil
}

func (s *Server) InjectDebugKey(ctx context.Context, req *runnyv1.InjectDebugKeyRequest) (*runnyv1.InjectDebugKeyResponse, error) {
	slot, err := s.findSlot(req.GetSlot())
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	pub, comment, _, rest, err := ssh.ParseAuthorizedKey([]byte(req.GetPublicKey()))
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "parsing public key: %v", err)
	}
	if len(rest) != 0 {
		return nil, status.Error(codes.InvalidArgument, "public_key must contain exactly one key")
	}

	hold := req.GetHold().AsDuration()
	holdCap := s.Config.Limits.MaxDebugHold.D()
	switch {
	case hold == 0:
		hold = holdCap
	case hold < 0:
		return nil, status.Error(codes.InvalidArgument, "hold must not be negative")
	case hold > holdCap:
		return nil, status.Errorf(codes.InvalidArgument, "hold %v exceeds limits.max_debug_hold (%v)", hold, holdCap)
	}

	st := slot.Status()
	if st.Wedged || (st.State != statemachine.StateListening &&
		st.State != statemachine.StateJob && st.State != statemachine.StateDebug) {
		return nil, status.Errorf(codes.FailedPrecondition,
			"slot %s is %s; key injection needs a live guest (LISTENING, JOB, or DEBUG)", slot.Name(), st.State)
	}

	// Re-marshal to the canonical "type base64" line: no client bytes ever
	// reach a guest shell.
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))
	fp := ssh.FingerprintSHA256(pub)
	queueBound, handlerWait := s.injectBounds()

	reply := make(chan statemachine.DebugKeyReply, 1)
	if !slot.Command(statemachine.Command{
		Kind: statemachine.CmdDebugKey, Reason: req.GetReason(),
		PubKey: line, Fingerprint: fp, Comment: comment, Hold: hold,
		CycleID: st.CycleID, SeenState: st.State, // both pins from the same read
		Expires: time.Now().Add(queueBound), Reply: reply,
	}) {
		return nil, status.Errorf(codes.Unavailable, "slot %s is not accepting commands", slot.Name())
	}

	select {
	case <-ctx.Done():
		return nil, status.FromContextError(ctx.Err()).Err()
	case <-time.After(handlerWait):
		return nil, status.Error(codes.DeadlineExceeded,
			"no response from the slot; nothing was injected and this request has expired — re-run debug")
	case r := <-reply:
		if r.Err != nil {
			return nil, status.Error(codes.FailedPrecondition, r.Err.Error())
		}
		resp := &runnyv1.InjectDebugKeyResponse{
			User: r.User, Ip: st.VM.IP, Fingerprint: fp,
			HostKeys: r.HostKeys, Armed: r.Armed,
		}
		if !r.Armed {
			resp.HoldUntil = timestamppb.New(r.HoldUntil)
		}
		return resp, nil
	}
}

// ---- proto conversion --------------------------------------------------------

var stateToProto = map[statemachine.State]runnyv1.SlotState{
	statemachine.StateBackoff:     runnyv1.SlotState_SLOT_STATE_BACKOFF,
	statemachine.StateEnsureImage: runnyv1.SlotState_SLOT_STATE_ENSURE_IMAGE,
	statemachine.StateClone:       runnyv1.SlotState_SLOT_STATE_CLONE,
	statemachine.StateBoot:        runnyv1.SlotState_SLOT_STATE_BOOT,
	statemachine.StateAwaitIP:     runnyv1.SlotState_SLOT_STATE_AWAIT_IP,
	statemachine.StateAwaitSSH:    runnyv1.SlotState_SLOT_STATE_AWAIT_SSH,
	statemachine.StateSecureSSH:   runnyv1.SlotState_SLOT_STATE_SECURE_SSH,
	statemachine.StateMintJIT:     runnyv1.SlotState_SLOT_STATE_MINT_JIT,
	statemachine.StateProvision:   runnyv1.SlotState_SLOT_STATE_PROVISION,
	statemachine.StateListening:   runnyv1.SlotState_SLOT_STATE_LISTENING,
	statemachine.StateJob:         runnyv1.SlotState_SLOT_STATE_JOB,
	statemachine.StateTeardown:    runnyv1.SlotState_SLOT_STATE_TEARDOWN,
	statemachine.StateDebug:       runnyv1.SlotState_SLOT_STATE_DEBUG,
}

func statusToProto(st statemachine.Status) *runnyv1.SlotStatus {
	out := &runnyv1.SlotStatus{
		Slot:                st.Slot,
		State:               stateToProto[st.State],
		StateEntered:        timestamppb.New(st.StateEntered),
		CycleId:             st.CycleID,
		RunnerName:          st.RunnerName,
		Image:               st.Image,
		ImageDigest:         st.ImageDigest,
		Paused:              st.Paused,
		ConsecutiveFailures: st.ConsecutiveFailures,
		BackoffSeconds:      st.BackoffSeconds,
		LastFailure:         st.LastFailure,
		Detail:              st.Detail,
		Wedged:              st.Wedged,
		DebugHoldArmed:      st.DebugHoldArmed,
	}
	if !st.DebugHoldExpires.IsZero() {
		out.DebugHoldExpires = timestamppb.New(st.DebugHoldExpires)
	}
	if st.VM.MAC != "" || st.VM.IP != "" {
		out.Vm = &runnyv1.VMInfo{Mac: st.VM.MAC, Ip: st.VM.IP}
	}
	if st.Job != nil {
		out.Job = &runnyv1.JobInfo{
			Name: st.Job.Name, Started: timestamppb.New(st.Job.Started),
			OperatorKeys: st.Job.OperatorKeys,
		}
	}
	return out
}

func recordToProto(r *cycle.Record) *runnyv1.CycleRecord {
	out := &runnyv1.CycleRecord{
		CycleId:     r.CycleID,
		Slot:        r.Slot,
		Image:       r.Image,
		ImageDigest: r.ImageDigest,
		Started:     timestamppb.New(r.Started),
		Finished:    timestamppb.New(r.Finished),
		Result:      string(r.Result),
		Vm:          &runnyv1.VMInfo{Mac: r.VM.MAC, Ip: r.VM.IP},
		Artifacts:   r.Artifacts,
	}
	for _, sr := range r.States {
		out.States = append(out.States, &runnyv1.StateRecord{
			State:   stateToProto[statemachine.State(sr.State)],
			Entered: timestamppb.New(sr.Entered),
			Left:    timestamppb.New(sr.Left),
			Outcome: string(sr.Outcome),
			Error:   sr.Error,
		})
	}
	if r.Job != nil {
		out.Job = &runnyv1.JobInfo{
			Name: r.Job.Name, Started: timestamppb.New(r.Job.Started),
			OperatorKeys: r.Job.OperatorKeys,
		}
	}
	for _, k := range r.InjectedKeys {
		out.InjectedKeys = append(out.InjectedKeys, &runnyv1.InjectedKey{
			Fingerprint: k.Fingerprint,
			Comment:     k.Comment,
			Injected:    timestamppb.New(k.Injected),
			Reason:      k.Reason,
			Outcome:     k.Outcome,
			Error:       k.Error,
			State:       k.State,
		})
	}
	if r.Failure != nil {
		out.FailureState = r.Failure.State
		out.FailureError = r.Failure.Error
	}
	return out
}
