//go:build windows

package vm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeReapSystem is a scriptable reapSystem: Terminate/WaitCtx/Close return
// canned values and record whether they were called.
type fakeReapSystem struct {
	id           string
	terminateErr error
	waitErr      error
	closeErr     error
	terminated   bool
	waited       bool
	closed       bool
}

func (f *fakeReapSystem) ID() string { return f.id }
func (f *fakeReapSystem) Terminate(context.Context) error {
	f.terminated = true
	return f.terminateErr
}

func (f *fakeReapSystem) WaitCtx(context.Context) error {
	f.waited = true
	return f.waitErr
}

func (f *fakeReapSystem) Close() error {
	f.closed = true
	return f.closeErr
}

// fakeReapEndpoint is a scriptable reapEndpoint.
type fakeReapEndpoint struct {
	id        string
	mac       string
	deleteErr error
	deleted   bool
}

func (f *fakeReapEndpoint) ID() string  { return f.id }
func (f *fakeReapEndpoint) MAC() string { return f.mac }
func (f *fakeReapEndpoint) Delete() error {
	f.deleted = true
	return f.deleteErr
}

// fakeReapOps is a scriptable reapOps: canned system/endpoint (or "not
// found"), and canned lookup errors.
type fakeReapOps struct {
	system    *fakeReapSystem // nil means "not found" unless systemErr is set
	systemErr error
	endpoint  *fakeReapEndpoint // nil means "not found" unless endpointErr is set
	endpntErr error

	openSystemCalls   int
	openEndpointCalls int
}

func (f *fakeReapOps) openSystem(context.Context, string) (reapSystem, error) {
	f.openSystemCalls++
	if f.systemErr != nil {
		return nil, f.systemErr
	}
	if f.system == nil {
		return nil, nil
	}
	return f.system, nil
}

func (f *fakeReapOps) openEndpoint(string) (reapEndpoint, error) {
	f.openEndpointCalls++
	if f.endpntErr != nil {
		return nil, f.endpntErr
	}
	if f.endpoint == nil {
		return nil, nil
	}
	return f.endpoint, nil
}

// TestReapPriorSystemNothingToReap: neither a stale system nor a stale
// endpoint exists -- the common, expected-rare-to-differ-from case. No
// error, nothing touched.
func TestReapPriorSystemNothingToReap(t *testing.T) {
	ops := &fakeReapOps{}
	if err := reapPriorSystem(ops, "slot-1", "runny-slot-1"); err != nil {
		t.Fatalf("reapPriorSystem = %v, want nil", err)
	}
	if ops.openSystemCalls != 1 || ops.openEndpointCalls != 1 {
		t.Errorf("openSystemCalls=%d openEndpointCalls=%d, want exactly one of each", ops.openSystemCalls, ops.openEndpointCalls)
	}
}

// TestReapPriorSystemProceedsWhenFound: a stale system AND endpoint both
// exist -- both get reaped, in the terminate-then-wait-then-close order
// (closing/deleting out from under a guest that might still be running is
// the mistake this ordering exists to avoid).
func TestReapPriorSystemProceedsWhenFound(t *testing.T) {
	sys := &fakeReapSystem{id: "slot-1"}
	ep := &fakeReapEndpoint{id: "ep-1", mac: "00:15:5d:00:00:01"}
	ops := &fakeReapOps{system: sys, endpoint: ep}

	if err := reapPriorSystem(ops, "slot-1", "runny-slot-1"); err != nil {
		t.Fatalf("reapPriorSystem = %v, want nil", err)
	}
	if !sys.terminated || !sys.waited || !sys.closed {
		t.Errorf("system lifecycle incomplete: terminated=%v waited=%v closed=%v, want all true", sys.terminated, sys.waited, sys.closed)
	}
	if !ep.deleted {
		t.Error("endpoint was not deleted")
	}
}

// TestReapPriorSystemWaitTimesOutFailsLoudly: Terminate only REQUESTS
// shutdown, so a WaitCtx that never confirms exit must fail loudly rather
// than let the caller close/delete out from under a guest that might still
// be running.
func TestReapPriorSystemWaitTimesOutFailsLoudly(t *testing.T) {
	sys := &fakeReapSystem{id: "slot-1", waitErr: context.DeadlineExceeded}
	ops := &fakeReapOps{system: sys}

	err := reapPriorSystem(ops, "slot-1", "runny-slot-1")
	if err == nil || !strings.Contains(err.Error(), "did not exit within the reap window") {
		t.Fatalf("reapPriorSystem = %v, want a reap-window-timeout error", err)
	}
	if sys.closed {
		t.Error("system was closed despite WaitCtx never confirming exit")
	}
}

// TestReapPriorSystemTerminateErrorIsNonFatal: Terminate failing (e.g. the
// system already exited on its own) must not abort the reap -- WaitCtx is
// still the authority on whether it's safe to proceed.
func TestReapPriorSystemTerminateErrorIsNonFatal(t *testing.T) {
	sys := &fakeReapSystem{id: "slot-1", terminateErr: context.Canceled}
	ops := &fakeReapOps{system: sys}

	if err := reapPriorSystem(ops, "slot-1", "runny-slot-1"); err != nil {
		t.Fatalf("reapPriorSystem = %v, want nil (terminate errors are logged, not fatal)", err)
	}
	if !sys.waited || !sys.closed {
		t.Error("reap did not proceed past a non-fatal terminate error")
	}
}

// TestReapPriorSystemOpenSystemErrorPropagates: a real check failure (not
// "not found") must surface, not be silently swallowed.
func TestReapPriorSystemOpenSystemErrorPropagates(t *testing.T) {
	wantErr := context.DeadlineExceeded
	ops := &fakeReapOps{systemErr: wantErr}

	err := reapPriorSystem(ops, "slot-1", "runny-slot-1")
	if err == nil || !strings.Contains(err.Error(), "checking for a stale compute system") {
		t.Fatalf("reapPriorSystem = %v, want a wrapped open-system error", err)
	}
}

// TestReapPriorSystemEndpointDeleteFailIsNonFatal: an endpoint delete
// failure is logged, not returned -- matches reapPriorSystem's own
// best-effort framing for the endpoint half.
func TestReapPriorSystemEndpointDeleteFailIsNonFatal(t *testing.T) {
	ep := &fakeReapEndpoint{id: "ep-1", mac: "00:15:5d:00:00:01", deleteErr: context.Canceled}
	ops := &fakeReapOps{endpoint: ep}

	if err := reapPriorSystem(ops, "slot-1", "runny-slot-1"); err != nil {
		t.Fatalf("reapPriorSystem = %v, want nil (endpoint delete failure is logged, not fatal)", err)
	}
	if !ep.deleted {
		t.Error("Delete was never attempted")
	}
}

// TestReapAllSlotsSkipsNonDirsAndReapsEachSlotDir: reapAllSlots enumerates
// vmsDir's SUBDIRECTORIES as slot/systemID names, ignoring stray files.
func TestReapAllSlotsSkipsNonDirsAndReapsEachSlotDir(t *testing.T) {
	vmsDir := t.TempDir()
	for _, name := range []string{"slot-a", "slot-b"} {
		if err := os.Mkdir(filepath.Join(vmsDir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(vmsDir, "stray-file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	seen := map[string]int{}
	ops := &countingReapOps{seen: seen}
	if err := reapAllSlots(ops, vmsDir); err != nil {
		t.Fatalf("reapAllSlots = %v, want nil", err)
	}
	if len(seen) != 2 || seen["slot-a"] != 1 || seen["slot-b"] != 1 {
		t.Errorf("reaped slots = %v, want exactly {slot-a:1, slot-b:1}", seen)
	}
}

// TestReapAllSlotsMissingVmsDirIsNoOp: a daemon's very first cold start (no
// vms/ yet) must not be treated as a reap failure.
func TestReapAllSlotsMissingVmsDirIsNoOp(t *testing.T) {
	if err := reapAllSlots(&fakeReapOps{}, filepath.Join(t.TempDir(), "does-not-exist")); err != nil {
		t.Fatalf("reapAllSlots = %v, want nil for a missing vms dir", err)
	}
}

// TestReapAllSlotsBestEffortContinuesPastAFailure: one slot's reap failing
// (its stale system never confirms exit) must not stop the others from
// being reaped -- a wedged orphan must not become a wedged daemon startup.
func TestReapAllSlotsBestEffortContinuesPastAFailure(t *testing.T) {
	vmsDir := t.TempDir()
	for _, name := range []string{"slot-a", "slot-b", "slot-c"} {
		if err := os.Mkdir(filepath.Join(vmsDir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	seen := map[string]int{}
	ops := &countingReapOps{seen: seen, failFor: "slot-b"}
	if err := reapAllSlots(ops, vmsDir); err != nil {
		t.Fatalf("reapAllSlots = %v, want nil (best-effort: per-slot failures are logged, not returned)", err)
	}
	if len(seen) != 3 {
		t.Errorf("reaped slots = %v, want all 3 attempted despite slot-b failing", seen)
	}
}

// TestReapAllSlotsRespectsOverallTimeout: a pass that runs out of its
// overall budget stops early rather than blocking startup indefinitely on
// an unbounded number of stuck slots.
func TestReapAllSlotsRespectsOverallTimeout(t *testing.T) {
	orig := reapOrphansTimeout
	reapOrphansTimeout = 20 * time.Millisecond
	t.Cleanup(func() { reapOrphansTimeout = orig })

	vmsDir := t.TempDir()
	for _, name := range []string{"slot-a", "slot-b", "slot-c", "slot-d"} {
		if err := os.Mkdir(filepath.Join(vmsDir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	seen := map[string]int{}
	ops := &countingReapOps{seen: seen, delay: 15 * time.Millisecond}
	if err := reapAllSlots(ops, vmsDir); err != nil {
		t.Fatalf("reapAllSlots = %v, want nil even when the budget runs out", err)
	}
	if len(seen) >= 4 {
		t.Errorf("reaped %d slots within a 20ms budget at 15ms/slot, want fewer than all 4 (early stop never triggered)", len(seen))
	}
}

// countingReapOps records which systemID each openSystem call was made for
// (reapAllSlots' own enumeration, not reapPriorSystem's per-call decision
// logic, which the fakeReapOps-based tests above already cover). failFor, if
// set, makes that one systemID's openSystem call fail. delay, if set, sleeps
// before returning -- used to drive the overall-timeout test.
type countingReapOps struct {
	seen    map[string]int
	failFor string
	delay   time.Duration
}

func (c *countingReapOps) openSystem(_ context.Context, systemID string) (reapSystem, error) {
	if c.delay > 0 {
		time.Sleep(c.delay)
	}
	c.seen[systemID]++
	if systemID == c.failFor {
		return nil, context.DeadlineExceeded
	}
	return nil, nil
}

func (c *countingReapOps) openEndpoint(string) (reapEndpoint, error) {
	return nil, nil
}
