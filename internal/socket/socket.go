// Package socket serves the runny.v1 control surface over the daemon's unix
// socket. runnyctl and RunnyBar are equal clients of this server.
package socket

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/user"
	"regexp"
	"runtime/debug"
	"strconv"
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

// DrainState is the structured drain status the server publishes on
// GetStatus, read from the drainer as a unit so the reason, the held flag, and
// the progress counter never disagree across separate calls.
type DrainState struct {
	// Reason is the human-readable drain reason, with the exit-gate hold
	// annotation appended when held; "" = not draining.
	Reason string
	// ExitHeld is true while the exit gate is refusing to exit onto a config
	// that no longer parses. Authoritative — clients gate on it, never re-parse
	// Reason.
	ExitHeld bool
	// Seq is a monotone drain-progress counter; a following client resets its
	// stall timer when it changes.
	Seq uint64
}

// WireProtocolVersion is the daemon's wire-contract version, published in
// GetStatusResponse.protocol_version. Bump it when the daemon gains a feature a
// client must detect before relying on it. Version 1 introduced pause/resume
// command acknowledgement (SlotStatus.recent_applied_command_ids): a client
// confirms a pause/resume from the command id only against a daemon advertising
// >= 1. Version 2 introduced reload-convergence confirmation (boot_id,
// config_sha256, drain_seq, exit_held): a reload-following client confirms the
// respawn by boot_id flip + config hash only against a daemon advertising >= 2,
// and otherwise falls back to daemon_started and warns it cannot verify.
const WireProtocolVersion uint32 = 2

// maxCommandIDLen bounds the optional pause/resume command id the client echoes
// back for acknowledgement. The app sends a UUID (36 chars); the cap is generous
// but finite because the daemon appends every applied id to the slot's
// recent_applied_command_ids history — an unbounded id from a malformed or
// hostile client (the socket is a trust boundary: every client is equal and
// unprivileged) would amplify into unbounded per-slot memory. Empty is
// allowed and means "don't track this command".
const maxCommandIDLen = 128

// validateCommandID rejects an oversized echoed command id before it can reach a
// slot's history. It is an argument check, so it runs ahead of any state gate
// (e.g. the drain gate): a malformed request is InvalidArgument regardless of
// daemon state.
func validateCommandID(id string) error {
	if len(id) > maxCommandIDLen {
		return status.Errorf(codes.InvalidArgument, "command_id is %d bytes; the limit is %d", len(id), maxCommandIDLen)
	}
	return nil
}

// PruneItem is one artifact the prune planner has identified for reclaim.
type PruneItem struct {
	Path   string
	Bytes  int64
	Kind   string // "runner-tarball" | "image-bundle"
	Reason string // "superseded" | "removed pool" | "dead .partial"
	Label  string
}

// PruneSkip records a configured image ref that was left intact because its
// registry digest could not be resolved.
type PruneSkip struct {
	Ref    string
	Reason string
}

// PrunePlan is the result returned by PruneFn: the item list, whether it was
// applied, any skips (refs whose digest could not be resolved), non-fatal
// scan errors, and a best-effort apply error.
type PrunePlan struct {
	Items          []PruneItem
	Applied        bool
	Skips          []PruneSkip
	Errors         []string // non-fatal scan/plan errors surfaced to the caller
	ReclaimedBytes int64    // bytes actually freed; only meaningful when Applied=true
	ApplyErr       error
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
	// BootID is this process's random per-cold-start identity (the respawn
	// discriminator), published as GetStatusResponse.boot_id and echoed into
	// ReloadResponse.accepting_boot_id. Minted in NewServer.
	BootID string
	// ConfigSHA256 is the hex SHA-256 of the config bytes this process loaded
	// at cold start, published as GetStatusResponse.config_sha256 so a reload
	// follower can prove the respawn came up on the vetted file. Set by main.
	ConfigSHA256 string
	// ReloadFn validates the on-disk config and (on acceptance) starts the
	// drain toward a respawn. It is called unconditionally — even while a
	// drain is already active, the verdict matters because the respawn loads
	// the on-disk file regardless. Nil = Unimplemented (handler unwired).
	ReloadFn func(ctx context.Context, reason string) ReloadResult
	// UpgradeReloadFn is the UpgradeReload-verb variant: it may defer a
	// config-parse failure to the respawn target's -test-config when the
	// running binary's own parser rejects a forward-only config. The verb is
	// the access boundary — plain Reload cannot defer. Nil = Unimplemented.
	UpgradeReloadFn func(ctx context.Context, reason string) ReloadResult
	// DrainFn reports the structured drain state (reason, held flag, progress
	// counter) as a unit. Nil = never draining.
	DrainFn func() DrainState
	// LocalNetworkGrantFn reports the daemon's live Local Network (TCC) grant
	// classification, published as GetStatusResponse.local_network_grant. The
	// daemon samples it off the hot path and caches it, so this reader is
	// non-blocking. Nil (non-darwin, or unwired) publishes UNSPECIFIED.
	LocalNetworkGrantFn func() runnyv1.LocalNetworkGrant
	// Config carries the limits InjectDebugKey validates against (hold cap,
	// the queue/service bounds for the synchronous wait).
	Config *home.Config
	// HomeDir is this daemon's resolved home (home.ResolveServer). Operator
	// grant/revoke/list reads and writes its ACL and operator-grants.jsonl.
	// Set by main.
	HomeDir home.Dir
	// IsSystemDaemon gates operator grant/revoke: a per-user home has a
	// single owner, not an ACL-managed set to mutate. Computed once in main
	// (where the resolveServer ownership check already ran) rather than
	// re-derived here by comparing HomeDir against home.SystemHomeDir — a
	// third copy of a comparison already made twice in cmd/runnyd/main.go.
	// Set by main.
	IsSystemDaemon bool
	// PruneFn builds a reclaim plan for stale image bundles and runner tarballs.
	// apply=true also deletes them. Nil = Unimplemented.
	PruneFn func(ctx context.Context, apply bool) PrunePlan

	// socketPath is the live control socket, set by Serve; GrantOperator and
	// RevokeOperator stamp it directly (inheritance from the home dir only
	// reaches a socket created AFTER the grant).
	socketPath string

	// gate is the per-RPC operator-revocation check, built by Serve from
	// IsSystemDaemon/HomeDir. nil (pass-through) on a per-user daemon, which
	// has no ACL-managed operator set to enforce against.
	gate *operatorGate

	// operatorMu serializes mutateOperator's List-then-mutate sequence:
	// without it, two concurrent grant/revoke RPCs (gRPC dispatches unary
	// calls on separate goroutines) can both read the same pre-mutation
	// operator set and both pass a precondition (e.g. "not the last
	// operator") that the other's mutation has already invalidated.
	operatorMu sync.Mutex

	// watches fans a status change out to every open WatchStatus call.
	watches *fanoutRegistry[chan struct{}]
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
		// A fresh random identity per cold start. crypto/rand.Text never fails
		// and yields 128+ bits of entropy — enough that two cold starts in a
		// tight crash-loop cannot collide and fool a follower into seeing the
		// old process. Distinct from the persisted, respawn-stable instance-id.
		BootID:  rand.Text(),
		Config:  cfg,
		watches: newFanoutRegistry[chan struct{}](),
	}
	for _, slot := range slots {
		slot.OnChange(func(statemachine.Status) { s.notify() })
	}
	return s
}

func (s *Server) notify() {
	s.watches.forEach(func(ch chan struct{}) {
		select {
		case ch <- struct{}{}:
		default:
		}
	})
}

// NotifyProgress is the exported seam the drainer wires as its progress hook: a
// drain_seq bump pushes a snapshot to watchers at once rather than waiting out
// the heartbeat. Same non-blocking fan-out as notify(), safe from an FSM goroutine.
func (s *Server) NotifyProgress() { s.notify() }

// recoveryUnary / recoveryStream convert a panicking handler into a recorded
// codes.Internal instead of crashing the daemon. grpc-go does not recover
// handler panics, and this socket is the unprivileged, every-client-equal
// control surface — one handler bug must not become a fleet-wide DoS that kills
// every slot's in-flight job. Visible-not-silent: the panic + stack is logged.
// Both share recoverPanic so the unary and stream paths can never diverge in
// what they log or return.
func recoverPanic(method string, errp *error) {
	if r := recover(); r != nil {
		slog.Error("recovered panic in gRPC handler", "method", method, "panic", r, "stack", string(debug.Stack()))
		*errp = status.Errorf(codes.Internal, "internal error")
	}
}

func recoveryUnary(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
	defer recoverPanic(info.FullMethod, &err)
	return handler(ctx, req)
}

func recoveryStream(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
	defer recoverPanic(info.FullMethod, &err)
	return handler(srv, ss)
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
	s.socketPath = socketPath
	s.gate = newOperatorGate(s.IsSystemDaemon, s.HomeDir.String())
	unaryChain := []grpc.UnaryServerInterceptor{recoveryUnary}
	streamChain := []grpc.StreamServerInterceptor{recoveryStream}
	if s.gate != nil {
		unaryChain = append(unaryChain, s.gate.unary)
		streamChain = append(streamChain, s.gate.stream)
	}
	g := grpc.NewServer(
		grpc.Creds(newPeerCreds()),
		grpc.ChainUnaryInterceptor(unaryChain...),
		grpc.ChainStreamInterceptor(streamChain...),
	)
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

// drainState is the nil-tolerant read of DrainFn.
func (s *Server) drainState() DrainState {
	if s.DrainFn == nil {
		return DrainState{}
	}
	return s.DrainFn()
}

func (s *Server) snapshot() *runnyv1.GetStatusResponse {
	ds := s.drainState()
	resp := &runnyv1.GetStatusResponse{
		DaemonStarted:   timestamppb.New(s.Started),
		Version:         s.Version,
		Draining:        ds.Reason,
		ProtocolVersion: WireProtocolVersion,
		BootId:          s.BootID,
		ConfigSha256:    s.ConfigSHA256,
		DrainSeq:        ds.Seq,
		ExitHeld:        ds.ExitHeld,
	}
	if s.LocalNetworkGrantFn != nil {
		resp.LocalNetworkGrant = s.LocalNetworkGrantFn()
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
	defer s.watches.register(ch)()

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
			return err
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
			return nil, status.Errorf(codes.InvalidArgument, "runner name %q is ambiguous between slots %v — use the slot name", name, names)
		}
	}
	return nil, status.Errorf(codes.NotFound, "no slot matches %q (use the slot name, or a runner name as shown by status)", name)
}

// command resolves a slot handle and injects cmd. The rejection is
// Unavailable (matching InjectDebugKey): a full command buffer drains itself
// within a cycle step, so retry-policy conventions treat it as retryable,
// unlike FailedPrecondition. The message names the RESOLVED slot — req may
// carry a runner name, and an error naming a nonexistent slot sends the
// operator grepping for the wrong thing.
func (s *Server) command(handle string, cmd statemachine.Command) error {
	slot, err := s.findSlot(handle)
	if err != nil {
		return err
	}
	if !slot.Command(cmd) {
		return status.Errorf(codes.Unavailable, "slot %s is not accepting commands", slot.Name())
	}
	return nil
}

func (s *Server) Recycle(ctx context.Context, req *runnyv1.RecycleRequest) (*runnyv1.RecycleResponse, error) {
	if err := s.command(req.GetSlot(), statemachine.Command{
		Kind: statemachine.CmdRecycle, Reason: req.GetReason(),
		CancelJob: req.GetCancelRunningJob(),
	}); err != nil {
		return nil, err
	}
	return &runnyv1.RecycleResponse{}, nil
}

func (s *Server) Pause(ctx context.Context, req *runnyv1.PauseRequest) (*runnyv1.PauseResponse, error) {
	if err := validateCommandID(req.GetCommandId()); err != nil {
		return nil, err
	}
	// A full command buffer (the drainer saturates non-converged slots with
	// re-issued pause+recycle pairs) must surface as an error, never a silent
	// drop reported as success — the silent-failure-proofness invariant.
	if err := s.command(req.GetSlot(), statemachine.Command{Kind: statemachine.CmdPause, ID: req.GetCommandId()}); err != nil {
		return nil, err
	}
	resp := &runnyv1.PauseResponse{}
	// Pause during a drain is allowed (idempotent; the drain wants slots
	// paused anyway) but the operator must learn it is in-memory: the
	// respawn at the drain's end silently clears it, a window that can last
	// hours (a running job finishes first).
	if d := s.drainState().Reason; d != "" {
		resp.Note = fmt.Sprintf("daemon is draining for restart (%s); pause is in-memory and will not survive the respawn", d)
	}
	return resp, nil
}

func (s *Server) Resume(ctx context.Context, req *runnyv1.ResumeRequest) (*runnyv1.ResumeResponse, error) {
	if err := validateCommandID(req.GetCommandId()); err != nil {
		return nil, err
	}
	// A resume mid-drain would silently fight the drainer (which re-issues
	// pause until convergence); refuse with the cause instead. The gate read
	// and the command enqueue are not atomic: drainer.Start can set d.reason
	// in between, so re-check after enqueuing and undo if a drain raced in.
	if d := s.drainState().Reason; d != "" {
		return nil, status.Errorf(codes.FailedPrecondition, "daemon is draining: %s; resume after the respawn", d)
	}
	if err := s.command(req.GetSlot(), statemachine.Command{Kind: statemachine.CmdResume, ID: req.GetCommandId()}); err != nil {
		return nil, err
	}
	if d := s.drainState().Reason; d != "" {
		// A drain started between the gate read and the command enqueue.
		// Best-effort undo: if the buffer is full the drainer's re-issue loop
		// (observe→recheck→CmdPause) covers the gap on the next FSM transition,
		// so a dropped undo does not permanently stall the drain.
		_ = s.command(req.GetSlot(), statemachine.Command{Kind: statemachine.CmdPause})
		return nil, status.Errorf(codes.FailedPrecondition, "daemon is draining: %s; resume after the respawn", d)
	}
	return &runnyv1.ResumeResponse{}, nil
}

// reloadResponse builds the proto response from a ReloadResult, stamping the
// accepting process's boot_id. Shared by Reload and UpgradeReload (same
// messages, same wire shape, different validation authority).
func (s *Server) reloadResponse(r ReloadResult) *runnyv1.ReloadResponse {
	resp := &runnyv1.ReloadResponse{
		Accepted:            r.Accepted,
		StartedDrain:        r.StartedDrain,
		Draining:            r.Draining,
		SlotCount:           int32(r.SlotCount),
		OperatorPausedSlots: r.OperatorPausedSlots,
		ConfigSha256:        r.ConfigSHA256,
		// The accepting process's own identity, captured in this round-trip so
		// a follower needs no pre-RPC status read to baseline against — the
		// respawn is then identified by a boot_id that differs from this one.
		AcceptingBootId: s.BootID,
	}
	for _, c := range r.FailedChecks {
		resp.FailedChecks = append(resp.FailedChecks, &runnyv1.DoctorCheck{Name: c.Name, Ok: c.OK, Detail: c.Detail})
	}
	for _, c := range r.Warnings {
		resp.Warnings = append(resp.Warnings, &runnyv1.DoctorCheck{Name: c.Name, Ok: c.OK, Detail: c.Detail})
	}
	return resp
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
	return s.reloadResponse(s.ReloadFn(ctx, req.GetReason())), nil
}

// UpgradeReload is the upgrade-daemon verb: it may defer a config-parse failure
// to the respawn target's -test-config when the running binary's own parser
// rejects a forward-only config edit. A plain Reload cannot defer — the verb is
// the access boundary. Pre-feature daemons (< this version) return Unimplemented,
// which upgrade-daemon catches and surfaces as a clear operator message.
func (s *Server) UpgradeReload(ctx context.Context, req *runnyv1.ReloadRequest) (*runnyv1.ReloadResponse, error) {
	if s.UpgradeReloadFn == nil {
		return nil, status.Error(codes.Unimplemented, "upgrade-reload is not wired on this server")
	}
	return s.reloadResponse(s.UpgradeReloadFn(ctx, req.GetReason())), nil
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

// usernameLookupBound caps how long InjectDebugKey waits on lookupUsername:
// os/user.LookupId has no context-aware variant and can stall on a
// directory-service-backed NSS (LDAP/AD-joined hosts), which must never
// delay an operator's actual "shell in now" access (ADR-0014) for the sake
// of a cosmetic display field.
const usernameLookupBound = 2 * time.Second

// lookupID is the seam lookupUsername calls; overridable in tests to
// simulate a stuck NSS lookup without a real syscall.
var lookupID = user.LookupId

// lookupInFlight bounds the lookup goroutines lookupUsername can leave
// running past usernameLookupBound to 1: LookupId cannot be cancelled once
// started, so a wedged directory service means an abandoned goroutine stays
// blocked indefinitely. Without this cap, every debug-key request during an
// outage would leak another one — unbounded, the exact failure class the
// timeout was meant to close. The single slot is released by the lookup
// goroutine itself when LookupId actually returns, not by the timeout, so a
// still-stuck lookup keeps occupying it and concurrent callers skip
// resolution entirely rather than piling on.
var lookupInFlight = make(chan struct{}, 1)

// lookupUsername resolves uid to a username, best-effort: "" on any
// resolution failure, on a usernameLookupBound timeout, or when a previous
// lookup is still stuck (see lookupInFlight).
func lookupUsername(uid uint32) string {
	select {
	case lookupInFlight <- struct{}{}:
	default:
		return ""
	}
	ch := make(chan string, 1)
	go func() {
		defer func() { <-lookupInFlight }()
		name := ""
		if u, err := lookupID(strconv.FormatUint(uint64(uid), 10)); err == nil {
			name = u.Username
		}
		ch <- name
	}()
	select {
	case name := <-ch:
		return name
	case <-time.After(usernameLookupBound):
		return ""
	}
}

// injectionAborted reports whether ctx already ended (canceled or deadline
// exceeded), converting it to the same gRPC status InjectDebugKey's
// post-enqueue select uses for the identical condition — so both call sites
// agree on what the client sees. nil means ctx is still live.
func injectionAborted(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return status.FromContextError(err).Err()
	}
	return nil
}

func (s *Server) InjectDebugKey(ctx context.Context, req *runnyv1.InjectDebugKeyRequest) (*runnyv1.InjectDebugKeyResponse, error) {
	slot, err := s.findSlot(req.GetSlot())
	if err != nil {
		// findSlot already returns a typed NotFound/InvalidArgument.
		return nil, err
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

	// The operator identity for the audit trail: the kernel-authenticated
	// peer uid (never client-supplied), and its username resolved
	// best-effort here so the FSM stays free of os/user.
	var operatorUID *uint32
	var operatorUser string
	if uid, ok := peerUID(ctx); ok {
		operatorUID = &uid
		operatorUser = lookupUsername(uid)
	}

	// The lookup above can take up to usernameLookupBound; a client that
	// canceled or hit its own deadline during that stall must not have a key
	// installed after being told the request ended.
	if err := injectionAborted(ctx); err != nil {
		return nil, err
	}

	reply := make(chan statemachine.DebugKeyReply, 1)
	if !slot.Command(statemachine.Command{
		Kind: statemachine.CmdDebugKey, Reason: req.GetReason(),
		PubKey: line, Fingerprint: fp, Comment: comment, Hold: hold,
		CycleID: st.CycleID, SeenState: st.State, // both pins from the same read
		Expires: time.Now().Add(queueBound), Reply: reply,
		OperatorUID: operatorUID, OperatorUser: operatorUser,
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

func (s *Server) Prune(ctx context.Context, req *runnyv1.PruneRequest) (*runnyv1.PruneResponse, error) {
	if s.PruneFn == nil {
		return nil, status.Error(codes.Unimplemented, "prune is not wired on this server")
	}
	plan := s.PruneFn(ctx, req.GetApply())
	resp := &runnyv1.PruneResponse{Applied: plan.Applied}
	for _, item := range plan.Items {
		if !plan.Applied {
			resp.ReclaimedBytes += item.Bytes // dry-run: estimate from plan
		}
		resp.Items = append(resp.Items, &runnyv1.ReclaimItem{
			Path: item.Path, Bytes: item.Bytes,
			Kind: item.Kind, Reason: item.Reason, Label: item.Label,
		})
	}
	if plan.Applied {
		resp.ReclaimedBytes = plan.ReclaimedBytes // actual: only successfully removed items
	}
	for _, skip := range plan.Skips {
		resp.Skips = append(resp.Skips, &runnyv1.PruneSkip{Ref: skip.Ref, Reason: skip.Reason})
	}
	resp.Errors = append(resp.Errors, plan.Errors...)
	if plan.ApplyErr != nil {
		resp.Errors = append(resp.Errors, plan.ApplyErr.Error())
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
		Slot:                    st.Slot,
		State:                   stateToProto[st.State],
		StateEntered:            timestamppb.New(st.StateEntered),
		CycleId:                 st.CycleID,
		RunnerName:              st.RunnerName,
		Image:                   st.Image,
		ImageDigest:             st.ImageDigest,
		RunnerVersion:           st.RunnerVersion,
		Paused:                  st.Paused,
		ConsecutiveFailures:     st.ConsecutiveFailures,
		BackoffSeconds:          st.BackoffSeconds,
		LastFailure:             st.LastFailure,
		Detail:                  st.Detail,
		Wedged:                  st.Wedged,
		DebugHoldArmed:          st.DebugHoldArmed,
		RecentAppliedCommandIds: st.RecentAppliedCommandIDs,
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
	out.ActiveCycleStates = stateRecordsToProto(st.ActiveCycleStates)
	return out
}

func stateRecordsToProto(records []cycle.StateRecord) []*runnyv1.StateRecord {
	if len(records) == 0 {
		return nil
	}
	out := make([]*runnyv1.StateRecord, 0, len(records))
	for _, sr := range records {
		out = append(out, &runnyv1.StateRecord{
			State:   stateToProto[statemachine.State(sr.State)],
			Entered: timestamppb.New(sr.Entered),
			Left:    timestamppb.New(sr.Left),
			Outcome: string(sr.Outcome),
			Error:   sr.Error,
		})
	}
	return out
}

func recordToProto(r *cycle.Record) *runnyv1.CycleRecord {
	out := &runnyv1.CycleRecord{
		CycleId:       r.CycleID,
		Slot:          r.Slot,
		Image:         r.Image,
		ImageDigest:   r.ImageDigest,
		RunnerVersion: r.RunnerVersion,
		Started:       timestamppb.New(r.Started),
		Finished:      timestamppb.New(r.Finished),
		Result:        string(r.Result),
		Ending:        string(r.Ending),
		Artifacts:     r.Artifacts,
		CycleDir:      r.CycleDir,
	}
	if r.VM.MAC != "" || r.VM.IP != "" {
		out.Vm = &runnyv1.VMInfo{Mac: r.VM.MAC, Ip: r.VM.IP}
	}
	out.States = stateRecordsToProto(r.States)
	if r.Job != nil {
		out.Job = &runnyv1.JobInfo{
			Name: r.Job.Name, Started: timestamppb.New(r.Job.Started),
			OperatorKeys: r.Job.OperatorKeys,
		}
	}
	for _, k := range r.InjectedKeys {
		out.InjectedKeys = append(out.InjectedKeys, &runnyv1.InjectedKey{
			Fingerprint:  k.Fingerprint,
			Comment:      k.Comment,
			Injected:     timestamppb.New(k.Injected),
			Reason:       k.Reason,
			Outcome:      k.Outcome,
			Error:        k.Error,
			State:        k.State,
			OperatorUid:  k.OperatorUID,
			OperatorUser: k.OperatorUser,
		})
	}
	if r.Failure != nil {
		out.FailureState = r.Failure.State
		out.FailureError = r.Failure.Error
	}
	return out
}
