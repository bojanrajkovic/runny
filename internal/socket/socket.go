// Package socket serves the runny.v1 control surface over the daemon's unix
// socket. runnyctl and RunnyBar are equal clients of this server (ADR-0006).
package socket

import (
	"context"
	"fmt"
	"net"
	"os"
	"slices"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/bojanrajkovic/runny/internal/cycle"
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

	// watch fan-out
	mu      sync.Mutex
	watchID int
	watches map[int]chan struct{}
}

// NewServer wires the slots' OnChange into the watch fan-out.
func NewServer(slots []*statemachine.Slot, ring, runnerRing *logring.Ring,
	stores func(string) cycle.Store, doctor func(context.Context) []DoctorCheck, version string,
) *Server {
	s := &Server{
		Slots:      slots,
		Ring:       ring,
		RunnerRing: runnerRing,
		Stores:     stores,
		DoctorFn:   doctor,
		Started:    time.Now(),
		Version:    version,
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

func (s *Server) snapshot() *runnyv1.GetStatusResponse {
	resp := &runnyv1.GetStatusResponse{
		DaemonStarted: timestamppb.New(s.Started),
		Version:       s.Version,
	}
	for _, slot := range s.Slots {
		resp.Slots = append(resp.Slots, statusToProto(slot.Status()))
	}
	return resp
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
		want := req.GetSlot()
		if !slices.ContainsFunc(s.Slots, func(sl *statemachine.Slot) bool { return sl.Name() == want }) {
			return status.Errorf(codes.NotFound, "no slot named %q", want)
		}
		keep = func(e logring.Entry) bool { return e.Attrs["slot"] == want }
	}
	// With a filter, replay counts matching lines: subscribe to the whole
	// buffer and tail the survivors, so `-replay 50 loupe-1` means 50 lines
	// of loupe-1, not loupe-1's share of the last 50 global lines.
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

func (s *Server) findSlot(name string) (*statemachine.Slot, error) {
	for _, slot := range s.Slots {
		if slot.Name() == name {
			return slot, nil
		}
	}
	return nil, fmt.Errorf("no such slot %q", name)
}

func (s *Server) Recycle(ctx context.Context, req *runnyv1.RecycleRequest) (*runnyv1.RecycleResponse, error) {
	slot, err := s.findSlot(req.GetSlot())
	if err != nil {
		return nil, err
	}
	if !slot.Command(statemachine.Command{Kind: statemachine.CmdRecycle, Reason: req.GetReason()}) {
		return nil, fmt.Errorf("slot %s is not accepting commands", req.GetSlot())
	}
	return &runnyv1.RecycleResponse{}, nil
}

func (s *Server) Pause(ctx context.Context, req *runnyv1.PauseRequest) (*runnyv1.PauseResponse, error) {
	slot, err := s.findSlot(req.GetSlot())
	if err != nil {
		return nil, err
	}
	slot.Command(statemachine.Command{Kind: statemachine.CmdPause})
	return &runnyv1.PauseResponse{}, nil
}

func (s *Server) Resume(ctx context.Context, req *runnyv1.ResumeRequest) (*runnyv1.ResumeResponse, error) {
	slot, err := s.findSlot(req.GetSlot())
	if err != nil {
		return nil, err
	}
	slot.Command(statemachine.Command{Kind: statemachine.CmdResume})
	return &runnyv1.ResumeResponse{}, nil
}

func (s *Server) Why(ctx context.Context, req *runnyv1.WhyRequest) (*runnyv1.WhyResponse, error) {
	if _, err := s.findSlot(req.GetSlot()); err != nil {
		return nil, err
	}
	n := int(req.GetCycles())
	if n == 0 {
		n = 1
	}
	recs, err := s.Stores(req.GetSlot()).Recent(n)
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

// ---- proto conversion --------------------------------------------------------

var stateToProto = map[statemachine.State]runnyv1.SlotState{
	statemachine.StateBackoff:     runnyv1.SlotState_SLOT_STATE_BACKOFF,
	statemachine.StateEnsureImage: runnyv1.SlotState_SLOT_STATE_ENSURE_IMAGE,
	statemachine.StateClone:       runnyv1.SlotState_SLOT_STATE_CLONE,
	statemachine.StateBoot:        runnyv1.SlotState_SLOT_STATE_BOOT,
	statemachine.StateAwaitIP:     runnyv1.SlotState_SLOT_STATE_AWAIT_IP,
	statemachine.StateAwaitSSH:    runnyv1.SlotState_SLOT_STATE_AWAIT_SSH,
	statemachine.StateMintJIT:     runnyv1.SlotState_SLOT_STATE_MINT_JIT,
	statemachine.StateProvision:   runnyv1.SlotState_SLOT_STATE_PROVISION,
	statemachine.StateListening:   runnyv1.SlotState_SLOT_STATE_LISTENING,
	statemachine.StateJob:         runnyv1.SlotState_SLOT_STATE_JOB,
	statemachine.StateTeardown:    runnyv1.SlotState_SLOT_STATE_TEARDOWN,
}

func statusToProto(st statemachine.Status) *runnyv1.SlotStatus {
	out := &runnyv1.SlotStatus{
		Slot:                st.Slot,
		State:               stateToProto[st.State],
		StateEntered:        timestamppb.New(st.StateEntered),
		CycleId:             st.CycleID,
		Paused:              st.Paused,
		ConsecutiveFailures: st.ConsecutiveFailures,
		BackoffSeconds:      st.BackoffSeconds,
		LastFailure:         st.LastFailure,
		Detail:              st.Detail,
		Wedged:              st.Wedged,
	}
	if st.VM.MAC != "" || st.VM.IP != "" {
		out.Vm = &runnyv1.VMInfo{Mac: st.VM.MAC, Ip: st.VM.IP}
	}
	if st.Job != nil {
		out.Job = &runnyv1.JobInfo{Name: st.Job.Name, Started: timestamppb.New(st.Job.Started)}
	}
	return out
}

func recordToProto(r *cycle.Record) *runnyv1.CycleRecord {
	out := &runnyv1.CycleRecord{
		CycleId:     r.CycleID,
		Slot:        r.Slot,
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
		out.Job = &runnyv1.JobInfo{Name: r.Job.Name, Started: timestamppb.New(r.Job.Started)}
	}
	if r.Failure != nil {
		out.FailureState = r.Failure.State
		out.FailureError = r.Failure.Error
	}
	return out
}
