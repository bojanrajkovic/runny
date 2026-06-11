package socket

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/bojanrajkovic/runny/internal/cycle"
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
		Slot:       "mac-1",
		State:      statemachine.StateTeardown,
		Detail:     "guest survived force-stop",
		Wedged:     true,
		CycleID:    "abcd1234",
		RunnerName: "host-a1b2c3d4-mac-1-abcd1234",
	}
	pb := statusToProto(st)
	if !pb.GetWedged() || pb.GetDetail() != st.Detail || pb.GetSlot() != "mac-1" {
		t.Errorf("statusToProto dropped fields: %+v", pb)
	}
	if pb.GetRunnerName() != st.RunnerName {
		t.Errorf("RunnerName dropped: %q", pb.GetRunnerName())
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
	return NewServer(slots, ring, logring.NewRing(16), stores, doctor, "test")
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
	if _, err := c.Recycle(t.Context(), &runnyv1.RecycleRequest{Slot: "nope"}); err == nil {
		t.Error("Recycle of unknown slot should error")
	}
	if _, err := c.Pause(t.Context(), &runnyv1.PauseRequest{Slot: "nope"}); err == nil {
		t.Error("Pause of unknown slot should error")
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

	if _, err := c.Why(t.Context(), &runnyv1.WhyRequest{Slot: "nope"}); err == nil {
		t.Error("Why of unknown slot should error")
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
