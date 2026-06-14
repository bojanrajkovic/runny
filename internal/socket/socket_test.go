package socket

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/bojanrajkovic/runny/internal/cycle"
	"github.com/bojanrajkovic/runny/internal/home"
	"github.com/bojanrajkovic/runny/internal/logring"
	"github.com/bojanrajkovic/runny/internal/statemachine"
	runnyv1 "github.com/bojanrajkovic/runny/proto/runny/v1"
)

// If a state is added to the FSM without a proto mapping, statusToProto
// silently degrades it to SLOT_STATE_UNSPECIFIED on the wire — this test
// makes that loud, keyed off the FSM's own state inventory.
func TestStateToProtoIsExhaustive(t *testing.T) {
	if len(stateToProto) != len(statemachine.States) {
		t.Errorf("stateToProto has %d entries, FSM has %d states", len(stateToProto), len(statemachine.States))
	}
	for _, st := range statemachine.States {
		if pb, ok := stateToProto[st]; !ok || pb == runnyv1.SlotState_SLOT_STATE_UNSPECIFIED {
			t.Errorf("state %s has no proto mapping (would render as UNSPECIFIED on the wire)", st)
		}
	}
}

func TestStatusToProtoCarriesWedgedAndDetail(t *testing.T) {
	st := statemachine.Status{
		Slot:        "mac-1",
		State:       statemachine.StateTeardown,
		Detail:      "guest survived force-stop",
		Wedged:      true,
		CycleID:     "abcd1234",
		RunnerName:  "host-a1b2c3d4-mac-1-abcd1234",
		Image:       "ghcr.io/test/image:1",
		ImageDigest: "sha256:fake",
	}
	pb := statusToProto(st)
	if !pb.GetWedged() || pb.GetDetail() != st.Detail || pb.GetSlot() != "mac-1" {
		t.Errorf("statusToProto dropped fields: %+v", pb)
	}
	if pb.GetRunnerName() != st.RunnerName {
		t.Errorf("RunnerName dropped: %q", pb.GetRunnerName())
	}
	if pb.GetImage() != st.Image || pb.GetImageDigest() != st.ImageDigest {
		t.Errorf("image fields dropped: image=%q digest=%q", pb.GetImage(), pb.GetImageDigest())
	}
}

// The pause/resume command acknowledgement (issue #66) must reach the wire.
func TestStatusToProtoCarriesLastAppliedCommandID(t *testing.T) {
	pb := statusToProto(statemachine.Status{Slot: "mac-1", LastAppliedCommandID: "cmd-abc"})
	if pb.GetLastAppliedCommandId() != "cmd-abc" {
		t.Errorf("last_applied_command_id dropped: %q", pb.GetLastAppliedCommandId())
	}
}

// recordToProto must carry the configured ref (intent) alongside the
// resolved digest (truth) — the post-mortem pair `runnyctl why` renders.
func TestRecordToProtoCarriesImage(t *testing.T) {
	r := &cycle.Record{
		CycleID:     "abcd1234",
		Slot:        "mac-1",
		Image:       "ghcr.io/test/image:1",
		ImageDigest: "sha256:fake",
	}
	pb := recordToProto(r)
	if pb.GetImage() != r.Image {
		t.Errorf("Image dropped: %q", pb.GetImage())
	}
	if pb.GetImageDigest() != r.ImageDigest {
		t.Errorf("ImageDigest dropped: %q", pb.GetImageDigest())
	}
}

func TestStatusToProtoCarriesDebugFields(t *testing.T) {
	until := time.Date(2026, 6, 12, 16, 0, 0, 0, time.UTC)
	armed := statusToProto(statemachine.Status{State: statemachine.StateJob, DebugHoldArmed: true})
	if !armed.GetDebugHoldArmed() {
		t.Error("DebugHoldArmed dropped")
	}
	if armed.GetDebugHoldExpires() != nil {
		t.Error("DebugHoldExpires should be unset when zero")
	}
	held := statusToProto(statemachine.Status{State: statemachine.StateDebug, DebugHoldExpires: until})
	if held.GetDebugHoldExpires() == nil || !held.GetDebugHoldExpires().AsTime().Equal(until) {
		t.Errorf("DebugHoldExpires dropped: %v", held.GetDebugHoldExpires())
	}
}

func TestRecordToProtoCarriesInjectedKeys(t *testing.T) {
	r := &cycle.Record{
		CycleID: "abcd1234", Slot: "mac-1",
		Job: &cycle.JobInfo{Name: "build", OperatorKeys: []string{"SHA256:abc"}},
		InjectedKeys: []cycle.InjectedKey{
			{Fingerprint: "SHA256:abc", Outcome: "armed", State: "JOB", Reason: "wedged"},
		},
	}
	pb := recordToProto(r)
	if len(pb.GetInjectedKeys()) != 1 {
		t.Fatalf("injected keys dropped: %+v", pb.GetInjectedKeys())
	}
	k := pb.GetInjectedKeys()[0]
	if k.GetFingerprint() != "SHA256:abc" || k.GetOutcome() != "armed" || k.GetState() != "JOB" {
		t.Errorf("injected key mangled: %+v", k)
	}
	if len(pb.GetJob().GetOperatorKeys()) != 1 || pb.GetJob().GetOperatorKeys()[0] != "SHA256:abc" {
		t.Errorf("operator keys dropped: %+v", pb.GetJob().GetOperatorKeys())
	}
}

func TestInjectDebugKeyValidation(t *testing.T) {
	c := dial(t, newTestServer(testSlots("mac-1"), nil, nil, nil))
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	sshPub, _ := ssh.NewPublicKey(pub)
	goodKey := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " op@host"

	// Bad key → InvalidArgument.
	_, err := c.InjectDebugKey(t.Context(), &runnyv1.InjectDebugKeyRequest{Slot: "mac-1", PublicKey: "not a key"})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("bad key: code = %v, want InvalidArgument", status.Code(err))
	}

	// Negative hold → InvalidArgument.
	_, err = c.InjectDebugKey(t.Context(), &runnyv1.InjectDebugKeyRequest{
		Slot: "mac-1", PublicKey: goodKey, Hold: durationpb.New(-time.Hour),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("negative hold: code = %v, want InvalidArgument", status.Code(err))
	}

	// Over-cap hold → InvalidArgument (cap is 2h in the test config).
	_, err = c.InjectDebugKey(t.Context(), &runnyv1.InjectDebugKeyRequest{
		Slot: "mac-1", PublicKey: goodKey, Hold: durationpb.New(3 * time.Hour),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("over-cap hold: code = %v, want InvalidArgument", status.Code(err))
	}

	// Valid key but the slot is in BACKOFF (never started) → FailedPrecondition.
	_, err = c.InjectDebugKey(t.Context(), &runnyv1.InjectDebugKeyRequest{Slot: "mac-1", PublicKey: goodKey})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("backoff precheck: code = %v, want FailedPrecondition", status.Code(err))
	}

	// Unknown slot → NotFound.
	_, err = c.InjectDebugKey(t.Context(), &runnyv1.InjectDebugKeyRequest{Slot: "nope", PublicKey: goodKey})
	if status.Code(err) != codes.NotFound {
		t.Errorf("unknown slot: code = %v, want NotFound", status.Code(err))
	}
}

func TestToLogLine(t *testing.T) {
	e := logring.Entry{
		Time:    time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC),
		Level:   "INFO",
		Message: "state",
		Attrs:   map[string]string{"slot": "mac-1"},
	}
	l := toLogLine(e)
	if l.GetMessage() != "state" || l.GetLevel() != "INFO" || l.GetAttrs()["slot"] != "mac-1" {
		t.Errorf("toLogLine dropped fields: %+v", l)
	}
	if !l.GetTime().AsTime().Equal(e.Time) {
		t.Errorf("time mangled: %v", l.GetTime().AsTime())
	}
}

// ---- RPC handler coverage (over a real gRPC pipe via bufconn) ----------------

func testSlots(names ...string) []*statemachine.Slot {
	var slots []*statemachine.Slot
	for _, n := range names {
		slots = append(slots, statemachine.NewSlot(n, statemachine.Deps{}))
	}
	return slots
}

// newTestServer builds a Server with sensible defaults; pass non-nil to
// override a specific seam.
func newTestServer(slots []*statemachine.Slot, ring *logring.Ring, stores func(string) cycle.Store, doctor func(context.Context) []DoctorCheck) *Server {
	if ring == nil {
		ring = logring.NewRing(16)
	}
	if stores == nil {
		stores = func(string) cycle.Store { return cycle.Store{SlotDir: "/nonexistent"} } // Recent → nil, nil
	}
	if doctor == nil {
		doctor = func(context.Context) []DoctorCheck { return nil }
	}
	cfg := &home.Config{}
	cfg.Limits.MaxDebugHold = home.Duration(2 * time.Hour)
	cfg.Limits.ReconcileInterval = home.Duration(60 * time.Second)
	cfg.Deadlines.SecureSSH = home.Duration(15 * time.Second)
	return NewServer(slots, ring, logring.NewRing(16), stores, doctor, "test", cfg)
}

// dial serves srv over an in-memory bufconn pipe and returns a connected
// client — the real gRPC wire path, not direct handler calls.
func dial(t *testing.T, srv *Server) runnyv1.RunnyServiceClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	g := grpc.NewServer()
	runnyv1.RegisterRunnyServiceServer(g, srv)
	go func() { _ = g.Serve(lis) }()
	t.Cleanup(g.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return runnyv1.NewRunnyServiceClient(conn)
}

func TestGetStatusReturnsConfiguredSlots(t *testing.T) {
	c := dial(t, newTestServer(testSlots("mac-1", "mac-2"), nil, nil, nil))
	resp, err := c.GetStatus(t.Context(), &runnyv1.GetStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetSlots()) != 2 {
		t.Errorf("got %d slots, want 2", len(resp.GetSlots()))
	}
	if resp.GetVersion() != "test" {
		t.Errorf("version = %q", resp.GetVersion())
	}
}

func TestCommandsResolveSlotByName(t *testing.T) {
	c := dial(t, newTestServer(testSlots("mac-1"), nil, nil, nil))

	// Known slot: each command RPC succeeds.
	if _, err := c.Recycle(t.Context(), &runnyv1.RecycleRequest{Slot: "mac-1", Reason: "x"}); err != nil {
		t.Errorf("Recycle known slot: %v", err)
	}
	if _, err := c.Pause(t.Context(), &runnyv1.PauseRequest{Slot: "mac-1"}); err != nil {
		t.Errorf("Pause known slot: %v", err)
	}
	if _, err := c.Resume(t.Context(), &runnyv1.ResumeRequest{Slot: "mac-1"}); err != nil {
		t.Errorf("Resume known slot: %v", err)
	}

	// Unknown slot: findSlot's error path (this regressed once — findSlot
	// matched the mutable Status().Slot, empty until a slot transitions).
	if _, err := c.Recycle(t.Context(), &runnyv1.RecycleRequest{Slot: "nope"}); status.Code(err) != codes.NotFound {
		t.Errorf("Recycle of unknown slot: code = %v, want NotFound", status.Code(err))
	}
	if _, err := c.Pause(t.Context(), &runnyv1.PauseRequest{Slot: "nope"}); status.Code(err) != codes.NotFound {
		t.Errorf("Pause of unknown slot: code = %v, want NotFound", status.Code(err))
	}
	if _, err := c.Resume(t.Context(), &runnyv1.ResumeRequest{Slot: "nope"}); status.Code(err) != codes.NotFound {
		t.Errorf("Resume of unknown slot: code = %v, want NotFound", status.Code(err))
	}
}

// A slot whose command buffer is full is not accepting commands; the RPCs
// must say so (Unavailable — a transient, retryable condition, matching
// InjectDebugKey) instead of succeeding while doing nothing — that silent
// failure is exactly what this project exists to kill.
func TestCommandsFailWhenSlotNotAccepting(t *testing.T) {
	slots := testSlots("mac-1")
	// No FSM goroutine drains test slots, so filling the buffer sticks.
	for slots[0].Command(statemachine.Command{Kind: statemachine.CmdPause}) {
	}
	c := dial(t, newTestServer(slots, nil, nil, nil))

	if _, err := c.Recycle(t.Context(), &runnyv1.RecycleRequest{Slot: "mac-1", Reason: "x"}); status.Code(err) != codes.Unavailable {
		t.Errorf("Recycle on full buffer: code = %v, want Unavailable", status.Code(err))
	}
	if _, err := c.Pause(t.Context(), &runnyv1.PauseRequest{Slot: "mac-1"}); status.Code(err) != codes.Unavailable {
		t.Errorf("Pause on full buffer: code = %v, want Unavailable", status.Code(err))
	}
	if _, err := c.Resume(t.Context(), &runnyv1.ResumeRequest{Slot: "mac-1"}); status.Code(err) != codes.Unavailable {
		t.Errorf("Resume on full buffer: code = %v, want Unavailable", status.Code(err))
	}
}

// Commands accept what status displays: a full runner name resolves to its
// embedded slot, current cycle or stale.
func TestCommandsResolveRunnerName(t *testing.T) {
	c := dial(t, newTestServer(testSlots("mac-1", "lin-1"), nil, nil, nil))
	if _, err := c.Recycle(t.Context(), &runnyv1.RecycleRequest{Slot: "host-a1b2c3d4-mac-1-e48657d0", Reason: "x"}); err != nil {
		t.Errorf("Recycle by runner name: %v", err)
	}
	// A runner name whose embedded slot doesn't exist still errors.
	if _, err := c.Recycle(t.Context(), &runnyv1.RecycleRequest{Slot: "host-a1b2c3d4-mac-9-e48657d0"}); status.Code(err) != codes.NotFound {
		t.Errorf("Recycle of runner name with unknown slot: code = %v, want NotFound", status.Code(err))
	}
	// Ambiguity (dashes make <prefix>-<slot> structurally uncertain) errors
	// instead of guessing: "...-b-1-<cycle8>" suffix-matches both slots.
	srv := newTestServer(testSlots("b-1", "a-b-1"), nil, nil, nil)
	if _, err := srv.findSlot("host-a-b-1-e48657d0"); status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("ambiguous runner name should be InvalidArgument, got %v", err)
	}
}

func TestWhyReturnsRecentCycle(t *testing.T) {
	dir := t.TempDir()
	store := cycle.Store{SlotDir: dir}
	if err := store.Write(&cycle.Record{
		CycleID: "abcd1234", Slot: "mac-1",
		Started: time.Now(), Finished: time.Now(), Result: cycle.ResultSuccess,
	}); err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(testSlots("mac-1"), nil, func(string) cycle.Store { return store }, nil)
	c := dial(t, srv)

	resp, err := c.Why(t.Context(), &runnyv1.WhyRequest{Slot: "mac-1", Cycles: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetCycles()) != 1 || resp.GetCycles()[0].GetCycleId() != "abcd1234" {
		t.Errorf("Why = %+v, want one cycle abcd1234", resp.GetCycles())
	}

	if _, err := c.Why(t.Context(), &runnyv1.WhyRequest{Slot: "nope"}); status.Code(err) != codes.NotFound {
		t.Errorf("Why of unknown slot: code = %v, want NotFound", status.Code(err))
	}
}

func TestDoctorPassesThroughChecks(t *testing.T) {
	doctor := func(context.Context) []DoctorCheck {
		return []DoctorCheck{{Name: "platform", OK: true, Detail: "darwin/arm64"}}
	}
	c := dial(t, newTestServer(testSlots("mac-1"), nil, nil, doctor))
	resp, err := c.Doctor(t.Context(), &runnyv1.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetChecks()) != 1 || !resp.GetChecks()[0].GetOk() || resp.GetChecks()[0].GetName() != "platform" {
		t.Errorf("Doctor = %+v", resp.GetChecks())
	}
}

// Reload without a wired ReloadFn must scream Unimplemented, not pretend an
// empty verdict.
func TestReloadUnwiredIsUnimplemented(t *testing.T) {
	c := dial(t, newTestServer(testSlots("mac-1"), nil, nil, nil))
	_, err := c.Reload(t.Context(), &runnyv1.ReloadRequest{})
	if status.Code(err) != codes.Unimplemented {
		t.Errorf("unwired Reload: code = %v, want Unimplemented", status.Code(err))
	}
}

func TestReloadRefusedCarriesFailedChecks(t *testing.T) {
	srv := newTestServer(testSlots("mac-1"), nil, nil, nil)
	srv.ReloadFn = func(ctx context.Context, reason string) ReloadResult {
		return ReloadResult{
			Accepted:     false,
			FailedChecks: []DoctorCheck{{Name: "config-parse", OK: false, Detail: "bad yaml"}},
		}
	}
	c := dial(t, srv)
	resp, err := c.Reload(t.Context(), &runnyv1.ReloadRequest{Reason: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetAccepted() {
		t.Error("refused reload reported accepted")
	}
	fc := resp.GetFailedChecks()
	if len(fc) != 1 || fc[0].GetName() != "config-parse" || fc[0].GetOk() || fc[0].GetDetail() != "bad yaml" {
		t.Errorf("failed_checks = %+v", fc)
	}
}

func TestReloadAcceptedCarriesVerdictFields(t *testing.T) {
	srv := newTestServer(testSlots("mac-1", "mac-2"), nil, nil, nil)
	var sawReason string
	srv.ReloadFn = func(ctx context.Context, reason string) ReloadResult {
		sawReason = reason
		return ReloadResult{
			Accepted:            true,
			StartedDrain:        true,
			Warnings:            []DoctorCheck{{Name: "local-network", OK: false, Detail: "flake"}},
			Draining:            "config reload (rpc): new image",
			SlotCount:           2,
			OperatorPausedSlots: []string{"mac-2"},
			ConfigSHA256:        "abc123",
		}
	}
	c := dial(t, srv)
	resp, err := c.Reload(t.Context(), &runnyv1.ReloadRequest{Reason: "new image"})
	if err != nil {
		t.Fatal(err)
	}
	if sawReason != "new image" {
		t.Errorf("ReloadFn saw reason %q", sawReason)
	}
	if !resp.GetAccepted() || !resp.GetStartedDrain() || resp.GetSlotCount() != 2 || resp.GetConfigSha256() != "abc123" {
		t.Errorf("verdict fields dropped: %+v", resp)
	}
	if resp.GetDraining() != "config reload (rpc): new image" {
		t.Errorf("draining = %q", resp.GetDraining())
	}
	if len(resp.GetOperatorPausedSlots()) != 1 || resp.GetOperatorPausedSlots()[0] != "mac-2" {
		t.Errorf("operator_paused_slots = %v", resp.GetOperatorPausedSlots())
	}
	if len(resp.GetWarnings()) != 1 || resp.GetWarnings()[0].GetName() != "local-network" {
		t.Errorf("warnings = %+v", resp.GetWarnings())
	}
}

// Reload must never gate on an active drain: the respawn loads the on-disk
// file regardless, so the verdict matters most then (design decision 8).
func TestReloadWhileDrainingStillCallsReloadFn(t *testing.T) {
	srv := newTestServer(testSlots("mac-1"), nil, nil, nil)
	srv.DrainingFn = func() string { return "wedged guest: a VM survived force-stop" }
	called := false
	srv.ReloadFn = func(ctx context.Context, reason string) ReloadResult {
		called = true
		return ReloadResult{
			Accepted:     false,
			FailedChecks: []DoctorCheck{{Name: "config-parse", OK: false, Detail: "bad yaml"}},
			Draining:     "wedged guest: a VM survived force-stop",
		}
	}
	c := dial(t, srv)
	resp, err := c.Reload(t.Context(), &runnyv1.ReloadRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Error("Reload gated on the drain state instead of calling ReloadFn")
	}
	if resp.GetDraining() == "" {
		t.Error("draining not surfaced on a refused-while-draining reload")
	}
}

func TestResumeRefusedWhileDraining(t *testing.T) {
	srv := newTestServer(testSlots("mac-1"), nil, nil, nil)
	srv.DrainingFn = func() string { return "config reload (rpc): x" }
	c := dial(t, srv)
	_, err := c.Resume(t.Context(), &runnyv1.ResumeRequest{Slot: "mac-1"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Resume while draining: code = %v, want FailedPrecondition", status.Code(err))
	}
	if !strings.Contains(status.Convert(err).Message(), "config reload (rpc): x") {
		t.Errorf("refusal lacks the drain reason: %v", err)
	}
}

// A full command buffer must surface as an error, never a silent drop
// reported as success — the drainer saturates non-converged slots with
// re-issued pause+recycle pairs, so a mid-drain operator pause can hit a full
// buffer. (No Run goroutine drains the 8-deep buffer in this test.)
func TestPauseFullBufferSurfacesError(t *testing.T) {
	slots := testSlots("mac-1")
	for i := 0; i < 8; i++ {
		slots[0].Command(statemachine.Command{Kind: statemachine.CmdPause})
	}
	srv := newTestServer(slots, nil, nil, nil)
	c := dial(t, srv)
	_, err := c.Pause(t.Context(), &runnyv1.PauseRequest{Slot: "mac-1"})
	if err == nil || !strings.Contains(status.Convert(err).Message(), "not accepting commands") {
		t.Errorf("saturated Pause: err = %v, want 'not accepting commands'", err)
	}
}

func TestPauseNoteOnlyWhileDraining(t *testing.T) {
	srv := newTestServer(testSlots("mac-1"), nil, nil, nil)
	c := dial(t, srv)
	resp, err := c.Pause(t.Context(), &runnyv1.PauseRequest{Slot: "mac-1"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetNote() != "" {
		t.Errorf("pause note while not draining: %q", resp.GetNote())
	}
	srv.DrainingFn = func() string { return "config reload (rpc): x" }
	resp, err = c.Pause(t.Context(), &runnyv1.PauseRequest{Slot: "mac-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.GetNote(), "will not survive the respawn") {
		t.Errorf("pause note while draining = %q", resp.GetNote())
	}
}

func TestSnapshotCarriesDraining(t *testing.T) {
	srv := newTestServer(testSlots("mac-1"), nil, nil, nil)
	if got := srv.snapshot().GetDraining(); got != "" {
		t.Errorf("draining = %q with no DrainingFn", got)
	}
	srv.DrainingFn = func() string { return "config reload (SIGHUP)" }
	if got := srv.snapshot().GetDraining(); got != "config reload (SIGHUP)" {
		t.Errorf("draining = %q", got)
	}
}

// The daemon must advertise the wire protocol version so a client can decide
// whether to rely on pause/resume command acknowledgement (issue #66).
func TestSnapshotCarriesProtocolVersion(t *testing.T) {
	srv := newTestServer(testSlots("mac-1"), nil, nil, nil)
	if got := srv.snapshot().GetProtocolVersion(); got != WireProtocolVersion {
		t.Errorf("protocol_version = %d, want %d", got, WireProtocolVersion)
	}
}

func TestWatchStatusSendsInitialThenOnNotify(t *testing.T) {
	srv := newTestServer(testSlots("mac-1"), nil, nil, nil)
	c := dial(t, srv)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	stream, err := c.WatchStatus(ctx, &runnyv1.WatchStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil { // initial snapshot
		t.Fatalf("initial snapshot: %v", err)
	}
	// A state change fans out through the watch registry.
	srv.notify()
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("snapshot after notify: %v", err)
	}
	// Cancelling the stream context ends the handler cleanly (and removes the
	// watch from the registry via its defer).
	cancel()
	if _, err := stream.Recv(); err == nil {
		t.Error("stream did not end after cancel")
	}
}

func TestStreamLogsReplaysThenFollows(t *testing.T) {
	ring := logring.NewRing(16)
	logger := slog.New(logring.NewHandler(io.Discard, slog.LevelDebug, ring))
	logger.Info("line one")
	logger.Info("line two")

	c := dial(t, newTestServer(testSlots("mac-1"), ring, nil, nil))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	stream, err := c.StreamLogs(ctx, &runnyv1.StreamLogsRequest{Replay: 2, Follow: true, Daemon: true})
	if err != nil {
		t.Fatal(err)
	}
	// The two buffered lines replay in order before following.
	for _, want := range []string{"line one", "line two"} {
		got, err := stream.Recv()
		if err != nil {
			t.Fatalf("replay recv: %v", err)
		}
		if got.GetMessage() != want {
			t.Errorf("replay = %q, want %q", got.GetMessage(), want)
		}
	}
	// A line logged after subscription follows through live.
	logger.Info("line three")
	got, err := stream.Recv()
	if err != nil {
		t.Fatalf("follow recv: %v", err)
	}
	if got.GetMessage() != "line three" {
		t.Errorf("follow = %q, want line three", got.GetMessage())
	}
}

// runnerEntry builds a runner-output ring entry the way runnyd's
// OnRunnerLine sink does.
func runnerEntry(slot, line string) logring.Entry {
	return logring.Entry{
		Time: time.Now(), Level: "runner", Message: line,
		Attrs: map[string]string{"slot": slot, "cycle": "cycle123"},
	}
}

// The default StreamLogs stream is runner output; a slot filter narrows it,
// and replay counts the filtered lines, not the slot's share of the global
// tail.
func TestStreamLogsRunnerDefaultAndSlotFilter(t *testing.T) {
	srv := newTestServer(testSlots("mac-1", "mac-2"), nil, nil, nil)
	for i := range 3 {
		srv.RunnerRing.Add(runnerEntry("mac-1", fmt.Sprintf("one-%d", i)))
		srv.RunnerRing.Add(runnerEntry("mac-2", fmt.Sprintf("two-%d", i)))
	}
	c := dial(t, srv)

	// Default (no daemon, no slot): all runner lines, interleaved.
	stream, err := c.StreamLogs(t.Context(), &runnyv1.StreamLogsRequest{Replay: 10})
	if err != nil {
		t.Fatal(err)
	}
	var all []string
	for {
		l, err := stream.Recv()
		if err != nil {
			break // snapshot mode closes after the replay
		}
		all = append(all, l.GetAttrs()["slot"]+":"+l.GetMessage())
	}
	if len(all) != 6 {
		t.Fatalf("interleaved replay = %d lines (%v), want 6", len(all), all)
	}

	// Slot filter: only mac-2's lines, replay counted post-filter.
	stream, err = c.StreamLogs(t.Context(), &runnyv1.StreamLogsRequest{Replay: 2, Slot: "mac-2"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for {
		l, err := stream.Recv()
		if err != nil {
			break
		}
		if l.GetAttrs()["slot"] != "mac-2" {
			t.Errorf("filtered stream leaked %v", l)
		}
		got = append(got, l.GetMessage())
	}
	if len(got) != 2 || got[0] != "two-1" || got[1] != "two-2" {
		t.Errorf("filtered tail = %v, want [two-1 two-2]", got)
	}
}

func TestStreamLogsRejectsBadRequests(t *testing.T) {
	c := dial(t, newTestServer(testSlots("mac-1"), nil, nil, nil))
	stream, err := c.StreamLogs(t.Context(), &runnyv1.StreamLogsRequest{Slot: "nope"})
	if err == nil {
		_, err = stream.Recv() // stream RPC errors surface on first Recv
	}
	if status.Code(err) != codes.NotFound {
		t.Errorf("unknown slot: code = %v, want NotFound", status.Code(err))
	}
	stream, err = c.StreamLogs(t.Context(), &runnyv1.StreamLogsRequest{Daemon: true, Slot: "mac-1"})
	if err == nil {
		_, err = stream.Recv()
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("daemon+slot: code = %v, want InvalidArgument", status.Code(err))
	}
}

// Resume's drain guard reads draining() before enqueuing CmdResume, and
// re-reads after (issue #53). Simulate drainer.Start racing in between:
// the first draining() call returns "", the second returns a reason. The
// handler must return FailedPrecondition and not silently leave the slot
// running against an active drain.
func TestResumeDrainRaceIsRefused(t *testing.T) {
	// Buffered channel of size 1: first draining() call fills it and returns
	// "" (pre-enqueue check passes); subsequent calls hit default and return
	// an active drain reason. Goroutine-safe without imports.
	called := make(chan struct{}, 1)
	srv := newTestServer(testSlots("mac-1"), nil, nil, nil)
	srv.DrainingFn = func() string {
		select {
		case called <- struct{}{}:
			return ""
		default:
			return "config reload (rpc): test"
		}
	}
	c := dial(t, srv)
	_, err := c.Resume(t.Context(), &runnyv1.ResumeRequest{Slot: "mac-1"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Resume mid-drain race: code = %v, want FailedPrecondition", status.Code(err))
	}
}
