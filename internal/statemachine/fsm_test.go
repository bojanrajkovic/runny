package statemachine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bojanrajkovic/runny/internal/bounded"
	"github.com/bojanrajkovic/runny/internal/cycle"
	"github.com/bojanrajkovic/runny/internal/github"
	"github.com/bojanrajkovic/runny/internal/home"
	"github.com/bojanrajkovic/runny/internal/obs"
	"github.com/bojanrajkovic/runny/internal/tart"
	"github.com/bojanrajkovic/runny/internal/vm"
)

// ---- fakes -----------------------------------------------------------------

type fakeImages struct {
	bundle tart.Bundle
	err    error
	// maxCalls > 0: calls beyond it block until ctx ends, so tests control
	// exactly how many cycles run. blockAll blocks every call (a stuck pull).
	maxCalls int
	blockAll bool
	calls    int
	mu       sync.Mutex
}

func (f *fakeImages) Ensure(ctx context.Context, report func(string), onDigestResolved func(string)) (string, string, tart.Bundle, error) {
	if report != nil {
		report("pulled 1.0 MiB at 1.0 MiB/s")
	}
	f.mu.Lock()
	f.calls++
	blocked := f.blockAll || (f.maxCalls > 0 && f.calls > f.maxCalls)
	err := f.err
	f.mu.Unlock()
	if blocked {
		<-ctx.Done()
		return "", "", "", ctx.Err()
	}
	// Only fire the callback when Ensure will succeed: models the real
	// Resolve-then-PullTo ordering where the callback fires iff the registry
	// round-trip completed (a resolve failure leaves the digest unset).
	if onDigestResolved != nil && err == nil {
		onDigestResolved("sha256:fake")
	}
	return "sha256:fake", "actions-runner-osx-arm64-2.320.0.tar.gz", f.bundle, err
}

type fakeMachine struct {
	mac     string
	ip      string
	ipErr   error
	stopErr error
	done    chan struct{}
	stopped bool
	mu      sync.Mutex
}

func (m *fakeMachine) MAC() string { return m.mac }
func (m *fakeMachine) WaitIP(ctx bounded.Context) (string, error) {
	if m.ipErr != nil {
		return "", m.ipErr
	}
	if m.ip == "" { // simulate never-arriving lease
		<-ctx.Done()
		return "", ctx.Err()
	}
	return m.ip, nil
}

func (m *fakeMachine) Stop(bounded.Context, time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopErr != nil {
		return m.stopErr // the guest survives force-stop
	}
	if !m.stopped {
		m.stopped = true
		close(m.done)
	}
	return nil
}
func (m *fakeMachine) Done() <-chan struct{} { return m.done }

type fakeVM struct {
	machine      *fakeMachine
	bootErr      error
	boots        int
	lastCacheDir string // BootOptions.RunnerShareDir from the most recent Boot
	mu           sync.Mutex
}

func (f *fakeVM) Boot(ctx bounded.Context, b tart.Bundle, o vm.BootOptions) (vm.Machine, error) {
	f.mu.Lock()
	f.boots++
	f.lastCacheDir = o.RunnerShareDir
	f.mu.Unlock()
	if f.bootErr != nil {
		return nil, f.bootErr
	}
	return f.machine, nil
}

func (f *fakeVM) lastRunnerCacheDir() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastCacheDir
}

type fakeProc struct {
	lines chan string
	code  int
	done  chan struct{}
	once  sync.Once

	waitMu    sync.Mutex
	waitCalls int
}

func newFakeProc() *fakeProc {
	return &fakeProc{lines: make(chan string, 16), done: make(chan struct{})}
}

func (p *fakeProc) Lines() <-chan string { return p.lines }
func (p *fakeProc) Wait() (int, error) {
	p.waitMu.Lock()
	p.waitCalls++
	p.waitMu.Unlock()
	<-p.done
	return p.code, nil
}

// Kill must not read p.code: it's written inside exit's sync.Once on another
// goroutine, so reading it here is a data race (and the value is discarded —
// exit is once-guarded, a no-op when the proc already exited). -1 is the
// "force-killed, no clean exit code" sentinel for the rare kill-before-exit.
func (p *fakeProc) Kill() { p.exit(-1) }

// waits reports how many times Wait was called — the post-job force-close path
// (§5.4 step 2) must NOT call Wait, or the FSM goroutine hangs up to
// max_debug_hold on an orphaned-fd pathology.
func (p *fakeProc) waits() int {
	p.waitMu.Lock()
	defer p.waitMu.Unlock()
	return p.waitCalls
}
func (p *fakeProc) say(s string) { p.lines <- s }
func (p *fakeProc) exit(code int) {
	p.once.Do(func() {
		p.code = code
		close(p.lines)
		close(p.done)
	})
}

type fakeGuest struct {
	proc      *fakeProc
	startErr  error
	diag      []byte
	diagErr   error
	diagBlock bool // PullDiag blocks until ctx expires (a wedged guest)
	pulled    bool
	goos      string
	runnerTar string // the tarball basename StartRunner was handed

	// Debug-key injection seam (issue #39).
	hostKeys      []string
	stopErr       error // StopRunner returns this (death unproven)
	stopCalls     int
	stopBlock     bool   // StopRunner blocks until ctx expires
	stopNoClose   bool   // proven kill but Lines stays open (orphaned-fd pathology)
	stopSayMarker string // before closing, StopRunner buffers this line (a job that raced the kill)
	installErr    error  // InstallAuthorizedKey returns this
	installErrSeq []error
	installCalls  int
	installedKeys []string
	redialErr     error
	redialCalls   int

	// PullDebugSession seam.
	sessionLog    []byte
	sessionErr    error
	sessionPulled bool

	mu sync.Mutex
}

func (g *fakeGuest) StartRunner(ctx context.Context, jit, goos, runnerTarball string) (Proc, error) {
	if g.startErr != nil {
		return nil, g.startErr
	}
	g.mu.Lock()
	g.goos = goos
	g.runnerTar = runnerTarball
	g.mu.Unlock()
	return g.proc, nil
}

func (g *fakeGuest) PullDiag(ctx bounded.Context) ([]byte, error) {
	g.mu.Lock()
	g.pulled = true
	block := g.diagBlock
	g.mu.Unlock()
	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return g.diag, g.diagErr
}
func (g *fakeGuest) Close() error { return nil }

func (g *fakeGuest) StopRunner(ctx bounded.Context) error {
	g.mu.Lock()
	g.stopCalls++
	block, err, noClose, marker, proc := g.stopBlock, g.stopErr, g.stopNoClose, g.stopSayMarker, g.proc
	g.mu.Unlock()
	if block {
		<-ctx.Done()
		return ctx.Err()
	}
	if err != nil {
		return err // death unproven: the proc stays alive
	}
	// A proven kill ends the runner LISTENER: its output channel normally
	// closes, which the freeze/tail drain waits for (the real sshd session dies
	// with run.sh). stopNoClose models the budget-expiry pathology — the
	// listener is proven dead (pgrep read-back succeeds) but an orphaned job
	// descendant still holds the inherited stdout fd, so Lines never closes.
	// stopSayMarker models a job that raced the LISTENING freeze: a marker is
	// buffered (after the step-2 drain-check already ran) then the channel
	// closes, so the post-kill drain (freeze step 4) sees the marker.
	if proc != nil && marker != "" {
		proc.say(marker)
	}
	if proc != nil && !noClose {
		proc.exit(0)
	}
	return nil
}

func (g *fakeGuest) InstallAuthorizedKey(ctx bounded.Context, line string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := g.installCalls
	g.installCalls++
	if n < len(g.installErrSeq) {
		if err := g.installErrSeq[n]; err != nil {
			return err
		}
	} else if g.installErr != nil {
		return g.installErr
	}
	g.installedKeys = append(g.installedKeys, line)
	return nil
}

func (g *fakeGuest) HostKeys() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.hostKeys
}

func (g *fakeGuest) PullDebugSession(ctx bounded.Context) ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sessionPulled = true
	return g.sessionLog, g.sessionErr
}

func (g *fakeGuest) sessionPulledOnce() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.sessionPulled
}

func (g *fakeGuest) Redial(ctx bounded.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.redialCalls++
	return g.redialErr
}

func (g *fakeGuest) stops() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stopCalls
}

func (g *fakeGuest) installs() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.installCalls
}

func (g *fakeGuest) redials() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.redialCalls
}

type fakeDialer struct {
	guest *fakeGuest
	err   error

	mu          sync.Mutex
	rotated     *fakeGuest // Rotate's return when set; nil hands back g
	rotateErr   error
	rotateBlock bool // Rotate blocks until ctx expiry (a wedged guest)
	rotateCalls int
	rotateGoos  string
}

func (d *fakeDialer) WaitFor(ctx bounded.Context, addr, goos string) (Guest, error) {
	if d.err != nil {
		return nil, d.err
	}
	return d.guest, nil
}

func (d *fakeDialer) Rotate(ctx bounded.Context, addr string, g Guest, goos string) (Guest, error) {
	d.mu.Lock()
	d.rotateCalls++
	d.rotateGoos = goos
	rotated, err, block := d.rotated, d.rotateErr, d.rotateBlock
	d.mu.Unlock()
	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	if rotated != nil {
		return rotated, nil
	}
	return g, nil
}

func (d *fakeDialer) rotations() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.rotateCalls
}

type fakeGitHub struct {
	mu         sync.Mutex
	registered map[string]bool
	deleted    []int64
	nextID     int64
	listErr    error
	deleteErr  error
	dropAll    bool
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{registered: map[string]bool{}, nextID: 100}
}

func (g *fakeGitHub) GenerateJITConfig(ctx bounded.Context, name string, labels []string, group int64) (*github.JITRunner, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.nextID++
	g.registered[name] = true
	return &github.JITRunner{RunnerID: g.nextID, EncodedJITConfig: "aml0"}, nil
}

func (g *fakeGitHub) ListRunners(ctx bounded.Context) ([]github.Runner, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.listErr != nil {
		return nil, g.listErr
	}
	if g.dropAll {
		return nil, nil
	}
	var rs []github.Runner
	id := int64(1)
	for name := range g.registered {
		rs = append(rs, github.Runner{ID: id, Name: name, Status: "online"})
		id++
	}
	return rs, nil
}

func (g *fakeGitHub) DeleteRunner(ctx bounded.Context, id int64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.deleteErr != nil {
		return g.deleteErr // the registration was not removed
	}
	g.deleted = append(g.deleted, id)
	return nil
}

// ---- harness ----------------------------------------------------------------

type harness struct {
	slot    *Slot
	vmF     *fakeVM
	proc    *fakeProc
	guest   *fakeGuest
	dialer  *fakeDialer
	gh      *fakeGitHub
	images  *fakeImages
	dir     home.Dir
	states  chan Status
	runDone chan struct{}

	linesMu     sync.Mutex
	runnerLines []string // "slot cycle line" per OnRunnerLine call

	eventsMu sync.Mutex
	events   []obs.Event // every obs event this slot has emitted, across all cycles

	cloneMu    sync.Mutex
	cloneFiles [][2]string // {src, dst} per CloneFile call

	// removeAllFn is the injectable RemoveAll seam; swap via setRemoveAll.
	removeAllFn atomic.Pointer[func(string) error]
}

// setRemoveAll swaps the RemoveAll seam for the harness's slot. The swap is
// atomic so it can race safely against the FSM goroutine calling RemoveAll.
// Restoring the original is the caller's responsibility (via t.Cleanup).
func (h *harness) setRemoveAll(fn func(string) error) {
	h.removeAllFn.Store(&fn)
}

func (h *harness) cloneFileCalls() [][2]string {
	h.cloneMu.Lock()
	defer h.cloneMu.Unlock()
	return slices.Clone(h.cloneFiles)
}

// start launches the slot and registers a cleanup that waits for Run to
// fully exit — teardown detaches from ctx by design, so the goroutine can
// outlive cancel() and must be joined before TempDir cleanup.
func (h *harness) start(t *testing.T) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	h.runDone = make(chan struct{})
	go func() {
		h.slot.Run(ctx)
		close(h.runDone)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.runDone:
		case <-time.After(10 * time.Second):
			t.Error("slot.Run did not exit after cancel — a state is unbounded")
		}
	})
	return cancel
}

func newHarness(t *testing.T, mutate func(*home.Config)) *harness {
	return newHarnessPool(t, mutate, nil)
}

func newHarnessPool(t *testing.T, mutate func(*home.Config), mutatePool func(*home.PoolConfig)) *harness {
	t.Helper()
	dir := home.Dir(t.TempDir())
	if err := dir.Ensure(); err != nil {
		t.Fatal(err)
	}
	cfg := &home.Config{}
	pool := home.PoolConfig{
		Name:          "runner",
		OS:            "darwin",
		Image:         "ghcr.io/test/image:1",
		Count:         1,
		Labels:        []string{"self-hosted"},
		RunnerGroupID: 1,
		Target:        home.TargetConfig{Owner: "o", Repo: "r"},
		// Mirror the production default: cycles flow through SECURE_SSH
		// unless a test opts out via mutatePool.
		SSHHardening: home.SSHHardeningRotate,
	}
	set := func(d *home.Duration, v time.Duration) { *d = home.Duration(v) }
	set(&cfg.Deadlines.Clone, time.Second)
	set(&cfg.Deadlines.Boot, time.Second)
	set(&cfg.Deadlines.AwaitIP, 500*time.Millisecond)
	set(&cfg.Deadlines.AwaitSSH, time.Second)
	set(&cfg.Deadlines.MintJIT, time.Second)
	set(&cfg.Deadlines.SecureSSH, time.Second)
	set(&cfg.Deadlines.Provision, time.Second)
	set(&cfg.Deadlines.Teardown, 2*time.Second)
	set(&cfg.Limits.MaxJobDuration, 2*time.Second)
	set(&cfg.Limits.MaxIdle, time.Hour)
	set(&cfg.Limits.BackoffBase, 10*time.Millisecond)
	set(&cfg.Limits.BackoffCap, 80*time.Millisecond)
	set(&cfg.Limits.ReconcileInterval, 50*time.Millisecond)
	cfg.Retention.CyclesPerSlot = 10
	cfg.Retention.MaxAge = home.Duration(time.Hour)
	if mutate != nil {
		mutate(cfg)
	}
	if mutatePool != nil {
		mutatePool(&pool)
	}

	proc := newFakeProc()
	h := &harness{
		vmF:    &fakeVM{machine: &fakeMachine{mac: "62:25:1b:05:97:bf", ip: "192.168.64.9", done: make(chan struct{})}},
		proc:   proc,
		guest:  &fakeGuest{proc: proc, diag: []byte("diag-tail")},
		gh:     newFakeGitHub(),
		images: &fakeImages{bundle: tart.Bundle(filepath.Join(string(dir), "images", "fake")), maxCalls: 1},
		dir:    dir,
		states: make(chan Status, 256),
	}
	defaultRemoveAll := func(path string) error { return os.RemoveAll(path) }
	h.removeAllFn.Store(&defaultRemoveAll)
	h.dialer = &fakeDialer{guest: h.guest}
	deps := Deps{
		Home:           dir,
		Config:         cfg,
		Pool:           pool,
		InstancePrefix: "runny",
		VM:             h.vmF,
		Images:         h.images,
		Clone: func(src tart.Bundle, dst string) error {
			return os.MkdirAll(dst, 0o755)
		},
		CloneFile: func(src, dst string) error {
			h.cloneMu.Lock()
			h.cloneFiles = append(h.cloneFiles, [2]string{src, dst})
			h.cloneMu.Unlock()
			return os.WriteFile(dst, nil, 0o600) // dir is created by cloneRunnerTarball
		},
		RemoveAll: func(path string) error { return (*h.removeAllFn.Load())(path) },
		GitHub:    h.gh,
		Dial:      h.dialer,
		OnRunnerLine: func(slot, cycleID, line string) {
			h.linesMu.Lock()
			h.runnerLines = append(h.runnerLines, slot+" "+cycleID+" "+line)
			h.linesMu.Unlock()
		},
		Events: func(e obs.Event) {
			h.eventsMu.Lock()
			h.events = append(h.events, e)
			h.eventsMu.Unlock()
		},
	}
	h.slot = NewSlot("runner-1", deps)
	h.slot.OnChange(func(st Status) {
		select {
		case h.states <- st:
		default:
		}
	})
	h.verifyObsParityOnCleanup(t)
	return h
}

// waitState blocks until the slot reports state (with timeout).
func (h *harness) waitState(t *testing.T, want State) Status {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case st := <-h.states:
			if st.State == want {
				return st
			}
		case <-deadline:
			t.Fatalf("never reached state %s (currently %s)", want, h.slot.Status().State)
		}
	}
}

func (h *harness) records(t *testing.T) []*cycle.Record {
	t.Helper()
	recs, err := cycle.Store{SlotDir: h.dir.SlotCyclesDir("runner-1")}.Recent(0, "")
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

// obsStateCoverage accumulates, across every test in this package's run,
// which States actually appeared in a cycle.Record whose obs events were
// verified to match it. TestMain fails the whole binary if any non-BACKOFF
// State never showed up here — the only way a state lands in the set is by
// being driven end-to-end AND passing assertStepEventsMatchRecord, so this
// catches both "nothing tests this state" and "something tests it but its
// StepEntered/StepLeft pair was never wired" without any source inspection.
var (
	obsStateCoverageMu sync.Mutex
	obsStateCoverage   = map[State]bool{}
)

// verifyObsParityOnCleanup registers a t.Cleanup that runs after every test
// built on this harness (LIFO: after h.start's own cancel-and-wait-for-
// runDone cleanup, since that's registered later, by the test itself,
// whenever it calls h.start). Once the slot is stopped, it reads every
// cycle.Record the test produced and asserts the obs stream matches it —
// automatically, for every existing and future test that uses newHarness,
// not only ones that opt in by calling the assertion helpers themselves.
func (h *harness) verifyObsParityOnCleanup(t *testing.T) {
	t.Cleanup(func() {
		if t.Failed() {
			return // the test already failed on its own terms; don't pile on
		}
		recs, err := cycle.Store{SlotDir: h.dir.SlotCyclesDir("runner-1")}.Recent(0, "")
		if err != nil {
			return
		}
		for _, rec := range recs {
			events := h.eventsForCycle(rec.CycleID)
			if len(events) == 0 {
				continue // no Events hook wired for this test (e.g. the nil-hook test)
			}
			assertStepEventsMatchRecord(t, events, rec)
			obsStateCoverageMu.Lock()
			for _, sr := range rec.States {
				obsStateCoverage[State(sr.State)] = true
			}
			obsStateCoverageMu.Unlock()
		}
	})
}

// TestMain gates the whole package's test run on obs event coverage: every
// State this FSM can report (statemachine.States, the same exhaustive list
// socket_test.go's proto-mapping check keys off) must have been driven
// through at least one verified cycle. A new state added to States without a
// test that reaches it — or reached by a test but missing its
// StepEntered/StepLeft wiring — fails the build here, not silently.
func TestMain(m *testing.M) {
	code := m.Run()
	if code == 0 {
		obsStateCoverageMu.Lock()
		var missing []State
		for _, st := range States {
			if st == StateBackoff {
				continue // never part of a cycle; no StateRecord exists for it either
			}
			if !obsStateCoverage[st] {
				missing = append(missing, st)
			}
		}
		obsStateCoverageMu.Unlock()
		if len(missing) > 0 {
			fmt.Fprintf(os.Stderr,
				"obs event coverage gap: no test drove these states through a verified StepEntered/StepLeft pair: %v\n"+
					"add a test scenario that reaches each one (see newHarness/verifyObsParityOnCleanup)\n", missing)
			code = 1
		}
	}
	os.Exit(code)
}

// eventsForCycle returns the obs events emitted for one cycle, in emission
// (Seq) order. A harness accumulates events across every cycle the slot
// runs (including a gated cycle that starts before a test's cancel lands),
// so assertions on one cycle's shape must filter to it first.
func (h *harness) eventsForCycle(cycleID string) []obs.Event {
	h.eventsMu.Lock()
	defer h.eventsMu.Unlock()
	var out []obs.Event
	for _, e := range h.events {
		if e.Cycle.CycleID == cycleID {
			out = append(out, e)
		}
	}
	return out
}

// assertStepEventsMatchRecord verifies that events carries exactly one
// StepEntered/StepLeft pair per rec.States entry, in the same order, with
// the same (state, outcome, error) triple — the event stream and cycle.json
// are built from the same code path and must never disagree about a
// cycle's shape (ADR-0024).
func assertStepEventsMatchRecord(t *testing.T, events []obs.Event, rec *cycle.Record) {
	t.Helper()
	var entered, left []obs.StepEvent
	for _, e := range events {
		switch e.Kind {
		case obs.KindStepEntered:
			entered = append(entered, *e.StepInfo)
		case obs.KindStepLeft:
			left = append(left, *e.StepInfo)
		}
	}
	if len(entered) != len(rec.States) {
		t.Fatalf("got %d StepEntered events, want %d (one per StateRecord): %+v", len(entered), len(rec.States), entered)
	}
	if len(left) != len(rec.States) {
		t.Fatalf("got %d StepLeft events, want %d (one per StateRecord): %+v", len(left), len(rec.States), left)
	}
	for i, sr := range rec.States {
		if entered[i].State != sr.State {
			t.Errorf("StepEntered[%d].State = %q, want %q", i, entered[i].State, sr.State)
		}
		got := left[i]
		if got.State != sr.State || got.Outcome != obs.Outcome(sr.Outcome) || got.Error != sr.Error {
			t.Errorf("StepLeft[%d] = %+v, want state=%q outcome=%q error=%q", i, got, sr.State, sr.Outcome, sr.Error)
		}
	}
}

// assertCycleFramed verifies the cycle's event stream starts with
// CycleStarted and ends with CycleFinished, and that CycleFinished's payload
// matches the record's own result/ending/failure fields.
func assertCycleFramed(t *testing.T, events []obs.Event, rec *cycle.Record) {
	t.Helper()
	if len(events) < 2 {
		t.Fatalf("got %d events, want at least CycleStarted+CycleFinished", len(events))
	}
	if events[0].Kind != obs.KindCycleStarted {
		t.Errorf("first event = %v, want CycleStarted", events[0].Kind)
	}
	last := events[len(events)-1]
	if last.Kind != obs.KindCycleFinished {
		t.Fatalf("last event = %v, want CycleFinished", last.Kind)
	}
	if last.Finish == nil {
		t.Fatal("CycleFinished payload is nil")
	}
	if last.Finish.Result != string(rec.Result) {
		t.Errorf("Finish.Result = %q, want %q", last.Finish.Result, rec.Result)
	}
	if last.Finish.Ending != string(rec.Ending) {
		t.Errorf("Finish.Ending = %q, want %q", last.Finish.Ending, rec.Ending)
	}
	wantState, wantErr := "", ""
	if rec.Failure != nil {
		wantState, wantErr = rec.Failure.State, rec.Failure.Error
	}
	if last.Finish.FailureState != wantState || last.Finish.Error != wantErr {
		t.Errorf("Finish failure = (%q, %q), want (%q, %q)", last.Finish.FailureState, last.Finish.Error, wantState, wantErr)
	}
}

// ---- tests -------------------------------------------------------------------

// TestRecordOperatorKeyNotifiesWatchers pins that recording an operator debug
// key pushes the updated audit to StreamStatus subscribers. The ambiguous
// install-failure path records the (possibly-installed) key and then only
// rewrites the cycle sidecar — without a notify here, a watching client never
// sees that a privileged key may be live on the guest until some unrelated
// status change happens to fire. A silently-withheld security fact is exactly
// the failure mode this project exists to kill.
func TestRecordOperatorKeyNotifiesWatchers(t *testing.T) {
	job := &cycle.JobInfo{Name: "build"}
	rec := &cycle.Record{Job: job}
	s := &Slot{}
	s.status.Job = job

	got := make(chan Status, 4)
	s.OnChange(func(st Status) { got <- st })

	const fp = "SHA256:operator-key"
	s.recordOperatorKey(rec, fp)

	select {
	case st := <-got:
		if st.Job == nil || !slices.Contains(st.Job.OperatorKeys, fp) {
			t.Fatalf("watcher snapshot Job = %+v, want OperatorKeys to contain %q", st.Job, fp)
		}
	default:
		t.Fatalf("recordOperatorKey did not notify watchers; operator key %q is invisible to StreamStatus", fp)
	}
}

func TestHappyCycleThroughJob(t *testing.T) {
	h := newHarness(t, nil)
	cancel := h.start(t)
	_ = cancel

	h.waitState(t, StateProvision)
	h.proc.say("√ Connected to GitHub")
	h.proc.say("2026-06-09: Listening for Jobs")
	h.waitState(t, StateListening)

	h.proc.say("Running job: build (mac, self-hosted)")
	st := h.waitState(t, StateJob)
	if st.Job == nil || !strings.Contains(st.Job.Name, "build") {
		t.Errorf("job info = %+v", st.Job)
	}
	// The status carries the GitHub-visible runner name of the live cycle.
	if want := "runny-runner-1-" + st.CycleID; st.RunnerName != want {
		t.Errorf("RunnerName = %q, want %q", st.RunnerName, want)
	}
	// The live cycle carries both the configured ref and the resolved digest.
	if st.Image != "ghcr.io/test/image:1" {
		t.Errorf("Image = %q, want the configured pool ref", st.Image)
	}
	if st.ImageDigest != "sha256:fake" {
		t.Errorf("ImageDigest = %q, want the resolved digest", st.ImageDigest)
	}

	h.proc.say("Job build completed with result: Succeeded")
	h.proc.exit(0)
	h.waitState(t, StateTeardown)
	// Assert on the BACKOFF-entry snapshot: a later Status() read races the
	// next gated cycle, whose cancellation re-increments the counter.
	st = h.waitState(t, StateBackoff)
	if st.ConsecutiveFailures != 0 {
		t.Error("failures should reset after success")
	}
	// BACKOFF clears the digest (no guest exists; a stale digest after an
	// image bump is misinformation) but keeps the slot-constant ref.
	if st.ImageDigest != "" {
		t.Errorf("ImageDigest = %q in BACKOFF, want cleared", st.ImageDigest)
	}
	if st.Image != "ghcr.io/test/image:1" {
		t.Errorf("Image = %q in BACKOFF, want the configured ref retained", st.Image)
	}
	cancel()
	<-h.runDone // store is quiescent only once Run exits

	// The gated second cycle may have started before cancel landed; its
	// canceled teardown writes a failure record (every started cycle is
	// accounted). This test owns exactly one success record.
	recs := h.records(t)
	if len(recs) == 0 || len(recs) > 2 {
		t.Fatalf("got %d records", len(recs))
	}
	var rec *cycle.Record
	for _, r := range recs {
		if r.Result == cycle.ResultSuccess {
			if rec != nil {
				t.Fatal("more than one success record")
			}
			rec = r
		}
	}
	if rec == nil {
		t.Fatalf("no success record in %d records", len(recs))
	}
	// Every line the FSM consumed — provision wait, listening, and job —
	// reached the runner-log sink, attributed to this slot and cycle.
	h.linesMu.Lock()
	lines := strings.Join(h.runnerLines, "\n")
	h.linesMu.Unlock()
	for _, want := range []string{"Connected to GitHub", "Listening for Jobs", "Running job:", "completed with result"} {
		if !strings.Contains(lines, want) {
			t.Errorf("runner-log sink missing %q in:\n%s", want, lines)
		}
	}
	if !strings.Contains(lines, "runner-1 "+rec.CycleID+" ") {
		t.Errorf("runner lines not attributed to slot+cycle:\n%s", lines)
	}
	// Any second record must be the canceled gated cycle, not a double
	// write of the success cycle under another result.
	for _, r := range recs {
		if r != rec && r.CycleID == rec.CycleID {
			t.Errorf("cycle %s recorded twice (second result %s)", rec.CycleID, r.Result)
		}
	}
	if rec.Job == nil {
		t.Error("job not recorded")
	}
	// The record carries intent (the configured ref) alongside truth (the
	// resolved digest).
	if rec.Image != "ghcr.io/test/image:1" {
		t.Errorf("record Image = %q, want the configured pool ref", rec.Image)
	}
	if rec.ImageDigest != "sha256:fake" {
		t.Errorf("record ImageDigest = %q, want the resolved digest", rec.ImageDigest)
	}
	// Job ran → JIT runner self-removes → no explicit deletion.
	if len(h.gh.deleted) != 0 {
		t.Errorf("deleted %v, want none after a completed job", h.gh.deleted)
	}
}

func TestLastFailureClearedOnListening(t *testing.T) {
	// A slot that fails before boot (ENSURE_IMAGE error) accumulates a
	// LastFailure. Once the next cycle boots successfully and reaches LISTENING,
	// LastFailure must clear immediately — not wait for finishCycle — because
	// LISTENING proves the pre-boot failure is resolved and the slot is healthy.
	h := newHarness(t, nil)
	h.images.err = errors.New("simulated pull failure") // first cycle fails at ENSURE_IMAGE
	h.images.maxCalls = 2                               // second call succeeds after we clear err
	cancel := h.start(t)
	defer cancel()

	h.waitState(t, StateEnsureImage)
	st := h.waitState(t, StateBackoff)
	if st.ConsecutiveFailures < 1 {
		t.Fatalf("setup: want ≥1 failure after ENSURE_IMAGE error, got %d", st.ConsecutiveFailures)
	}
	if st.LastFailure == "" {
		t.Fatal("setup: LastFailure empty after ENSURE_IMAGE error, want failure message")
	}

	// Second cycle: let image pull succeed and the slot reach LISTENING.
	h.images.mu.Lock()
	h.images.err = nil
	h.images.mu.Unlock()

	h.waitState(t, StateProvision)
	h.proc.say(markerListening)
	st = h.waitState(t, StateListening)

	if st.LastFailure != "" {
		t.Errorf("LastFailure = %q after LISTENING entry, want empty", st.LastFailure)
	}
	if st.ConsecutiveFailures != 0 {
		t.Errorf("ConsecutiveFailures = %d after LISTENING entry, want 0", st.ConsecutiveFailures)
	}
}

func TestDeadlineFailureTearsDownAndBacksOff(t *testing.T) {
	h := newHarness(t, nil)
	h.images.maxCalls = 2
	h.vmF.machine.ip = "" // lease never arrives → AWAIT_IP deadline
	cancel := h.start(t)
	_ = cancel

	h.waitState(t, StateAwaitIP)
	h.waitState(t, StateTeardown)
	st := h.waitState(t, StateBackoff)
	if st.ConsecutiveFailures != 1 {
		t.Errorf("failures = %d, want 1", st.ConsecutiveFailures)
	}
	// Second cycle fails too; backoff grows.
	h.waitState(t, StateTeardown)
	st = h.waitState(t, StateBackoff)
	if st.ConsecutiveFailures != 2 {
		t.Errorf("failures = %d, want 2", st.ConsecutiveFailures)
	}
	cancel()

	recs := h.records(t)
	if len(recs) < 2 {
		t.Fatalf("got %d records", len(recs))
	}
	rec := recs[0]
	if rec.Result != cycle.ResultFailure || rec.Failure.State != string(StateAwaitIP) {
		t.Errorf("failure = %+v", rec.Failure)
	}
	var found bool
	for _, sr := range rec.States {
		if sr.State == string(StateAwaitIP) && sr.Outcome == cycle.OutcomeDeadline {
			found = true
		}
	}
	if !found {
		t.Errorf("AWAIT_IP not recorded as deadline: %+v", rec.States)
	}
	// The machine booted, so teardown must have stopped it.
	if !h.vmF.machine.stopped {
		t.Error("teardown did not stop the machine")
	}
	// Runner was never minted — nothing to delete.
	if len(h.gh.deleted) != 0 {
		t.Errorf("deleted %v", h.gh.deleted)
	}
}

func TestZombieDetectionRecycles(t *testing.T) {
	h := newHarness(t, nil)
	cancel := h.start(t)
	_ = cancel

	h.waitState(t, StateProvision)
	h.proc.say(markerListening)
	h.waitState(t, StateListening)

	// Registration vanishes (the DNS-blip-deregistration class).
	h.gh.mu.Lock()
	h.gh.dropAll = true
	h.gh.mu.Unlock()

	h.waitState(t, StateTeardown)
	h.waitState(t, StateBackoff)
	cancel()

	recs := h.records(t)
	if recs[0].Failure == nil || !strings.Contains(recs[0].Failure.Error, "zombie") {
		t.Errorf("failure = %+v", recs[0].Failure)
	}
	// No job ran → the (zombie) registration gets an explicit delete attempt.
	if len(h.gh.deleted) != 1 {
		t.Errorf("deleted = %v, want exactly the minted runner", h.gh.deleted)
	}
}

func TestGitHubOutageDoesNotRecycle(t *testing.T) {
	h := newHarness(t, nil)
	cancel := h.start(t)
	_ = cancel

	h.waitState(t, StateProvision)
	h.proc.say(markerListening)
	h.waitState(t, StateListening)

	// API unreachable: transient — must hold LISTENING, not recycle.
	h.gh.mu.Lock()
	h.gh.listErr = errors.New("api.github.com: connection refused")
	h.gh.mu.Unlock()

	time.Sleep(300 * time.Millisecond) // several reconcile ticks
	if st := h.slot.Status().State; st != StateListening {
		t.Errorf("state = %s after API outage, want LISTENING held", st)
	}
	cancel()
}

func TestPostMortemCollectedOnFailure(t *testing.T) {
	h := newHarness(t, nil)
	cancel := h.start(t)
	_ = cancel

	h.waitState(t, StateProvision)
	h.proc.exit(2) // runner dies before listening
	h.waitState(t, StateBackoff)
	cancel()

	recs := h.records(t)
	rec := recs[0]
	if rec.Result != cycle.ResultFailure {
		t.Fatalf("result = %s", rec.Result)
	}
	if !h.guest.pulled {
		t.Error("post-mortem was not pulled before destruction")
	}
	if len(rec.Artifacts) != 1 || rec.Artifacts[0] != "runner-diag.log" {
		t.Errorf("artifacts = %v", rec.Artifacts)
	}
	// And the artifact file actually exists next to cycle.json.
	dir, _ := (cycle.Store{SlotDir: h.dir.SlotCyclesDir("runner-1")}).Dir(rec)
	if _, err := os.Stat(filepath.Join(dir, "runner-diag.log")); err != nil {
		t.Errorf("runner-diag.log missing: %v", err)
	}
}

func TestOperatorRecycle(t *testing.T) {
	h := newHarness(t, nil)
	cancel := h.start(t)
	_ = cancel

	h.waitState(t, StateProvision)
	h.proc.say(markerListening)
	h.waitState(t, StateListening)

	if !h.slot.Command(Command{Kind: CmdRecycle, Reason: "image bump"}) {
		t.Fatal("command rejected")
	}
	h.waitState(t, StateTeardown)
	st := h.waitState(t, StateBackoff)
	if st.ConsecutiveFailures != 0 {
		t.Errorf("failures = %d after operator recycle; an operator action is not a health signal", st.ConsecutiveFailures)
	}
	cancel()

	recs := h.records(t)
	if !strings.Contains(recs[0].Failure.Error, "recycled by operator") {
		t.Errorf("failure = %+v", recs[0].Failure)
	}
	if recs[0].Ending != cycle.EndingRecycle {
		t.Errorf("Ending = %q, want %q", recs[0].Ending, cycle.EndingRecycle)
	}
}

// TestEndingShutdown pins that a cycle interrupted by daemon shutdown (the
// outer ctx cancelled, not an operator recycle) records Ending "shutdown" —
// distinct from "recycle" even though both are benign for backoff.
func TestEndingShutdown(t *testing.T) {
	h := newHarness(t, nil)
	cancel := h.start(t)

	h.waitState(t, StateProvision)
	h.proc.say(markerListening)
	h.waitState(t, StateListening)

	// Cancel the daemon-lifetime ctx directly (not CmdRecycle): this is the
	// shutdown path, ctx.Err() != nil, distinct from errOperatorRecycle.
	cancel()
	<-h.runDone

	recs := h.records(t)
	if len(recs) == 0 {
		t.Fatal("no records")
	}
	rec := recs[0]
	if rec.Result != cycle.ResultFailure {
		t.Fatalf("result = %s, want failure (the timeline stays truthful)", rec.Result)
	}
	if rec.Ending != cycle.EndingShutdown {
		t.Errorf("Ending = %q, want %q", rec.Ending, cycle.EndingShutdown)
	}
}

// TestEndingDebugExpiryBeatsShutdown pins the classification precedence when
// a daemon shutdown lands during teardown of a cycle whose DEBUG hold already
// expired: failErr was fixed at errDebugExpired before the shutdown arrived,
// so the record keeps Ending "failure" (whose verdict carries the expiry
// text) instead of being relabeled "shutdown".
func TestEndingDebugExpiryBeatsShutdown(t *testing.T) {
	h := newHarness(t, nil)
	h.images.maxCalls = 1
	cancel := h.reachListening(t)

	// Deterministic mid-teardown shutdown: after DEBUG, the only RemoveAll
	// is teardown's clone removal, which runs before the classification.
	h.setRemoveAll(func(path string) error {
		cancel()
		return os.RemoveAll(path)
	})

	if r := h.debugCmd(t, func(c *Command) { c.Hold = 50 * time.Millisecond }); r.Err != nil {
		t.Fatalf("freeze: %v", r.Err)
	}
	h.waitState(t, StateDebug)
	<-h.runDone

	recs := h.records(t)
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	rec := recs[0]
	// Precondition: the cycle really ended on the expiry, not something else.
	if rec.Failure == nil || !strings.Contains(rec.Failure.Error, "debug hold expired") {
		t.Fatalf("failure = %+v, want the debug expiry", rec.Failure)
	}
	if rec.Ending != cycle.EndingFailure {
		t.Errorf("Ending = %q, want %q — the expiry ended this cycle before the shutdown arrived", rec.Ending, cycle.EndingFailure)
	}
}

// TestEndingDebugRacedJobBeatsShutdown pins the branch's other sentinel: a
// job that raced the LISTENING freeze and died with the verified kill,
// followed by a shutdown during teardown, keeps Ending "failure" for the
// same reason as the expiry above.
func TestEndingDebugRacedJobBeatsShutdown(t *testing.T) {
	h := newHarness(t, nil)
	h.images.maxCalls = 1
	h.guest.stopSayMarker = "Running job: raced"
	cancel := h.reachListening(t)

	h.setRemoveAll(func(path string) error {
		cancel()
		return os.RemoveAll(path)
	})

	if r := h.debugCmd(t, nil); r.Err == nil || !strings.Contains(r.Err.Error(), "killed by the freeze") {
		t.Fatalf("want the raced-kill refusal, got %v", r.Err)
	}
	<-h.runDone

	recs := h.records(t)
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	rec := recs[0]
	if rec.Failure == nil || !strings.Contains(rec.Failure.Error, "job raced the debug freeze") {
		t.Fatalf("failure = %+v, want the raced-job kill", rec.Failure)
	}
	if rec.Ending != cycle.EndingFailure {
		t.Errorf("Ending = %q, want %q", rec.Ending, cycle.EndingFailure)
	}
}

// TestEndingPlainFailureBeatsShutdown pins the same precedence for a plain
// failure: a runner that dies on its own seals the cycle's fate before
// teardown starts, so a shutdown landing during teardown must not relabel
// the record "shutdown" (hiding a real health signal), nor exempt it from
// the failure streak as benign.
func TestEndingPlainFailureBeatsShutdown(t *testing.T) {
	h := newHarness(t, nil)
	h.images.maxCalls = 1
	cancel := h.reachListening(t)

	h.setRemoveAll(func(path string) error {
		cancel()
		return os.RemoveAll(path)
	})

	// A real failure, not shutdown-caused: the runner exits while idle.
	h.proc.exit(1)
	<-h.runDone

	recs := h.records(t)
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	rec := recs[0]
	if rec.Failure == nil || !strings.Contains(rec.Failure.Error, "runner exited") {
		t.Fatalf("failure = %+v, want the runner exit", rec.Failure)
	}
	if rec.Ending != cycle.EndingFailure {
		t.Errorf("Ending = %q, want %q — the runner died before the shutdown arrived", rec.Ending, cycle.EndingFailure)
	}
	if st := h.slot.Status(); st.ConsecutiveFailures != 1 {
		t.Errorf("a real failure must count even when shutdown races its teardown; failures=%d", st.ConsecutiveFailures)
	}
}

// teardownRecord returns the TEARDOWN StateRecord from a cycle record.
func teardownRecord(t *testing.T, r *cycle.Record) cycle.StateRecord {
	t.Helper()
	for _, sr := range r.States {
		if sr.State == string(StateTeardown) {
			return sr
		}
	}
	t.Fatalf("no TEARDOWN state in record %+v", r)
	return cycle.StateRecord{}
}

// A teardown that destroys the guest but fails a best-effort cleanup — the
// GitHub deregistration and/or the clone deletion — must record TEARDOWN as a
// warn naming the failure, never a bare ok: a cycle.json that swears teardown
// was clean while an orphan registration or clone lingers is the silent-record
// gap (#151). The warn is non-fatal — it must not escalate the slot's failure
// streak, since the local destruction succeeded and the orphan self-heals on
// the next cold-start sweep.
// TestEndingSuccess pins that a clean cycle records Ending "success" — the
// baseline every other ending class is judged against.
func TestEndingSuccess(t *testing.T) {
	h := newHarness(t, nil)
	cancel := h.start(t)
	_ = cancel

	h.waitState(t, StateProvision)
	h.proc.say(markerListening)
	h.waitState(t, StateListening)
	h.proc.say("Running job: build")
	h.waitState(t, StateJob)
	h.proc.say("Job build completed with result: Succeeded")
	h.proc.exit(0)
	h.waitState(t, StateTeardown)
	h.waitState(t, StateBackoff)
	cancel()
	<-h.runDone

	var rec *cycle.Record
	for _, r := range h.records(t) {
		if r.Result == cycle.ResultSuccess {
			rec = r
			break
		}
	}
	if rec == nil {
		t.Fatal("no success record")
	}
	if rec.Ending != cycle.EndingSuccess {
		t.Errorf("Ending = %q, want %q", rec.Ending, cycle.EndingSuccess)
	}
}

// TestEndingFailure pins that a per-state failure (not operator- or
// shutdown-caused) records Ending "failure" — distinguishable from a benign
// recycle/shutdown even though Result is "failure" in both cases.
func TestEndingFailure(t *testing.T) {
	h := newHarness(t, nil)
	h.vmF.machine.ip = "" // lease never arrives → AWAIT_IP deadline
	cancel := h.start(t)
	_ = cancel

	h.waitState(t, StateAwaitIP)
	h.waitState(t, StateTeardown)
	h.waitState(t, StateBackoff)
	cancel()

	recs := h.records(t)
	rec := recs[0]
	if rec.Result != cycle.ResultFailure {
		t.Fatalf("result = %s", rec.Result)
	}
	if rec.Ending != cycle.EndingFailure {
		t.Errorf("Ending = %q, want %q", rec.Ending, cycle.EndingFailure)
	}
}

func TestTeardownRecordsFailedCleanupsAsWarn(t *testing.T) {
	h := newHarness(t, nil)
	h.gh.deleteErr = errors.New("github 500")

	cancel := h.start(t)
	_ = cancel

	h.waitState(t, StateProvision)
	h.proc.say(markerListening)
	h.waitState(t, StateListening)

	// Fail the clone deletion, but only now — CLONE's pre-clone cleanup of
	// vms/<slot>/ routes through the same removeAll seam and has already run by
	// LISTENING, so injecting earlier would fail the cycle at CLONE instead of
	// exercising teardown's best-effort path.
	h.setRemoveAll(func(string) error { return errors.New("clone busy") })
	t.Cleanup(func() { h.setRemoveAll(os.RemoveAll) })

	// No job ran, but a runner is registered → teardown deregisters (and fails).
	if !h.slot.Command(Command{Kind: CmdRecycle, Reason: "image bump"}) {
		t.Fatal("command rejected")
	}
	h.waitState(t, StateTeardown)
	st := h.waitState(t, StateBackoff)
	if st.ConsecutiveFailures != 0 {
		t.Errorf("failures = %d; a recorded cleanup warn must not escalate the streak", st.ConsecutiveFailures)
	}
	cancel()

	// Target the operator-recycle cycle by content: backoff may elapse and a
	// trailing cycle start before cancel() lands, so its position is not fixed.
	var rec *cycle.Record
	for _, r := range h.records(t) {
		if r.Failure != nil && strings.Contains(r.Failure.Error, "recycled by operator") {
			rec = r
			break
		}
	}
	if rec == nil {
		t.Fatal("no operator-recycle cycle in records")
	}
	tr := teardownRecord(t, rec)
	if tr.Outcome != cycle.OutcomeWarn {
		t.Errorf("teardown outcome = %q, want %q (failed cleanups recorded as a warn)", tr.Outcome, cycle.OutcomeWarn)
	}
	if !strings.Contains(tr.Error, "github 500") || !strings.Contains(tr.Error, "clone busy") {
		t.Errorf("teardown error = %q, want both cleanup failures named", tr.Error)
	}
}

// A recycle issued mid-state must interrupt the cycle now — it once sat
// queued until LISTENING, hours later, then killed whatever healthy runner
// had come up in the meantime.
func TestRecycleInterruptsStuckPull(t *testing.T) {
	h := newHarness(t, nil)
	h.images.blockAll = true // the pull never finishes on its own
	cancel := h.start(t)
	_ = cancel

	h.waitState(t, StateEnsureImage)
	if !h.slot.Command(Command{Kind: CmdRecycle, Reason: "stuck pull"}) {
		t.Fatal("command rejected")
	}
	h.waitState(t, StateTeardown)
	st := h.waitState(t, StateBackoff)
	if st.ConsecutiveFailures != 0 {
		t.Errorf("failures = %d, want 0: operator recycle is benign", st.ConsecutiveFailures)
	}
	cancel()
	<-h.runDone

	recs := h.records(t)
	var found bool
	for _, r := range recs {
		if r.Failure != nil && strings.Contains(r.Failure.Error, "recycled by operator: stuck pull") {
			found = true
		}
	}
	if !found {
		t.Errorf("no record names the operator recycle; records: %+v", recs)
	}
}

func TestPauseHoldsInBackoff(t *testing.T) {
	h := newHarness(t, nil)
	h.images.maxCalls = 0 // unlimited cycles; pause must stop them
	h.images.err = errors.New("keep failing so we cycle fast")
	cancel := h.start(t)
	_ = cancel

	h.waitState(t, StateBackoff)
	h.slot.Command(Command{Kind: CmdPause})
	// Drain to the paused backoff state.
	deadline := time.After(3 * time.Second)
	for {
		st := h.slot.Status()
		if st.State == StateBackoff && st.Paused {
			break
		}
		select {
		case <-deadline:
			t.Fatal("never settled into paused BACKOFF")
		case <-time.After(20 * time.Millisecond):
		}
	}
	boots := func() int {
		h.vmF.mu.Lock()
		defer h.vmF.mu.Unlock()
		return h.vmF.boots
	}
	before := boots()
	time.Sleep(300 * time.Millisecond)
	if got := boots(); got != before {
		t.Errorf("boots advanced %d→%d while paused", before, got)
	}

	h.slot.Command(Command{Kind: CmdResume})
	time.Sleep(300 * time.Millisecond)
	if h.slot.Status().Paused {
		t.Error("still paused after resume")
	}
	cancel()
}

// A pause/resume carrying a command id publishes it as the acknowledgement
// (issue #66), and an id-less daemon-internal re-issue must not clobber it.
func TestPauseResumeAcknowledgeCommandID(t *testing.T) {
	h := newHarness(t, nil)
	h.images.maxCalls = 0 // unlimited cycles; we drive it to paused BACKOFF
	h.images.err = errors.New("keep failing so we cycle fast")
	cancel := h.start(t)
	defer cancel()

	h.waitState(t, StateBackoff)

	waitAck := func(want func(Status) bool, desc string) {
		t.Helper()
		deadline := time.After(3 * time.Second)
		for {
			if want(h.slot.Status()) {
				return
			}
			select {
			case <-deadline:
				st := h.slot.Status()
				t.Fatalf("never %s (paused=%v recent_applied=%q)", desc, st.Paused, st.RecentAppliedCommandIDs)
			case <-time.After(20 * time.Millisecond):
			}
		}
	}

	// Pause with an id records it.
	h.slot.Command(Command{Kind: CmdPause, ID: "pause-1"})
	waitAck(func(s Status) bool { return s.Paused && slices.Contains(s.RecentAppliedCommandIDs, "pause-1") },
		"acknowledged pause id pause-1")

	// An id-less re-issue (the drainer saturates paused slots with re-pauses)
	// must NOT append — an empty id is never recorded, and a coalesced status
	// stream must keep carrying the identified id so the client still confirms.
	h.slot.Command(Command{Kind: CmdPause})
	time.Sleep(200 * time.Millisecond)
	if ids := h.slot.Status().RecentAppliedCommandIDs; !slices.Equal(ids, []string{"pause-1"}) {
		t.Fatalf("id-less re-issue altered acknowledgement history: got %q, want [pause-1]", ids)
	}

	// Resume with a fresh id appends it; the prior pause id stays in history.
	h.slot.Command(Command{Kind: CmdResume, ID: "resume-1"})
	waitAck(func(s Status) bool { return !s.Paused && slices.Contains(s.RecentAppliedCommandIDs, "resume-1") },
		"acknowledged resume id resume-1")
	if ids := h.slot.Status().RecentAppliedCommandIDs; !slices.Equal(ids, []string{"pause-1", "resume-1"}) {
		t.Fatalf("history lost an id: got %q, want [pause-1 resume-1]", ids)
	}
}

// appendBounded keeps at most cap entries, evicting oldest-first, and never
// mutates a slice a prior snapshot already holds.
func TestAppendBoundedEvictsOldest(t *testing.T) {
	var ids []string
	for i := range 20 {
		ids = appendBounded(ids, fmt.Sprintf("id-%d", i), recentCommandIDCap)
	}
	if len(ids) != recentCommandIDCap {
		t.Fatalf("len = %d, want %d", len(ids), recentCommandIDCap)
	}
	if ids[0] != "id-4" || ids[recentCommandIDCap-1] != "id-19" {
		t.Fatalf("window = %q, want [id-4 .. id-19]", ids)
	}

	// A snapshot taken before a further append must keep its own backing array.
	snap := appendBounded([]string{"a", "b"}, "c", recentCommandIDCap)
	_ = appendBounded(snap, "d", recentCommandIDCap)
	if !slices.Equal(snap, []string{"a", "b", "c"}) {
		t.Fatalf("a later append mutated a prior snapshot: %q", snap)
	}
}

// A pause issued in a running state (the inline switch path, not the BACKOFF
// idle handler) publishes its command id immediately, even though the pause
// itself only takes hold at the next BACKOFF.
func TestPauseInListeningAcknowledgesCommandID(t *testing.T) {
	h := newHarness(t, nil)
	cancel := h.start(t)
	defer cancel()

	h.waitState(t, StateProvision)
	h.proc.say(markerListening)
	h.waitState(t, StateListening)

	h.slot.Command(Command{Kind: CmdPause, ID: "listen-pause"})
	deadline := time.After(2 * time.Second)
	for {
		if slices.Contains(h.slot.Status().RecentAppliedCommandIDs, "listen-pause") {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("LISTENING pause never acknowledged id: got %q", h.slot.Status().RecentAppliedCommandIDs)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func TestRunnerExitWhileListeningFails(t *testing.T) {
	h := newHarness(t, nil)
	cancel := h.start(t)
	_ = cancel

	h.waitState(t, StateProvision)
	h.proc.say(markerListening)
	h.waitState(t, StateListening)
	h.proc.exit(1)
	h.waitState(t, StateTeardown)
	h.waitState(t, StateBackoff)
	cancel()

	recs := h.records(t)
	if !strings.Contains(recs[0].Failure.Error, "exited") {
		t.Errorf("failure = %+v", recs[0].Failure)
	}
}

// A guest that survives force-stop must wedge the slot: TEARDOWN recorded
// as an error (not OK), the clone bundle kept as evidence, the slot parked.
// Regression: this case once recorded OutcomeOK, deleted the disk out from
// under the live guest, and burned doomed boot cycles against the occupied
// guest cap forever.
func TestStopFailureWedgesSlot(t *testing.T) {
	h := newHarness(t, nil)
	h.vmF.machine.stopErr = errors.New("force stop failed with guest still running")
	h.vmF.machine.ip = "" // fail at AWAIT_IP so teardown owns a booted machine
	cancel := h.start(t)
	_ = cancel

	h.waitState(t, StateTeardown)
	// The slot must park on its own — Run exits without ctx cancellation.
	select {
	case <-h.runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("slot did not park after the guest survived force-stop")
	}

	st := h.slot.Status()
	if !st.Wedged {
		t.Error("status.Wedged = false, want true")
	}
	// A wedged slot RETAINS its digest: the zombie occupying a guest-cap
	// slot is literally still executing that image, and showing it is the
	// honest answer. Pinned so a future "clear everything on park" refactor
	// trips loudly.
	if st.ImageDigest != "sha256:fake" {
		t.Errorf("ImageDigest = %q after wedge, want retained — the surviving guest still runs it", st.ImageDigest)
	}
	if h.vmF.boots != 1 {
		t.Errorf("boots = %d; a wedged slot must not retry boots", h.vmF.boots)
	}

	recs := h.records(t)
	rec := recs[0]
	if rec.Result != cycle.ResultFailure {
		t.Errorf("result = %s, want failure", rec.Result)
	}
	if rec.Ending != cycle.EndingWedge {
		t.Errorf("Ending = %q, want %q — the wedge outranks the AWAIT_IP failure that preceded it", rec.Ending, cycle.EndingWedge)
	}
	var tr *cycle.StateRecord
	for i := range rec.States {
		if rec.States[i].State == string(StateTeardown) {
			tr = &rec.States[i]
		}
	}
	if tr == nil || tr.Outcome != cycle.OutcomeError || !strings.Contains(tr.Error, "stop escalation failed") {
		t.Errorf("teardown record = %+v, want error outcome with the stop failure", tr)
	}
	// The undead guest still holds the clone bundle; it must not be deleted.
	if _, err := os.Stat(h.dir.VMDir("runner-1")); err != nil {
		t.Errorf("vm dir was deleted out from under a live guest: %v", err)
	}
}

// TestSuccessThenWedgeIsWedge pins the ok && wedged corner: a cycle whose job
// completed cleanly but whose teardown could not kill the guest is a wedge,
// not a success — the zombie occupies a guest-cap slot regardless of how well
// the job went. Result is failure (truthful timeline), Ending is wedge (not
// success, which the pre-teardown cycle earned), and the failure streak
// escalates: a wedge is never benign.
func TestSuccessThenWedgeIsWedge(t *testing.T) {
	h := newHarness(t, nil)
	h.vmF.machine.stopErr = errors.New("force stop failed with guest still running")
	cancel := h.start(t)
	_ = cancel

	h.waitState(t, StateProvision)
	h.proc.say(markerListening)
	h.waitState(t, StateListening)
	h.proc.say("Running job: build")
	h.waitState(t, StateJob)
	h.proc.say("Job build completed with result: Succeeded")
	h.proc.exit(0)
	h.waitState(t, StateTeardown)

	// The slot must park on its own — Run exits without ctx cancellation.
	select {
	case <-h.runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("slot did not park after the guest survived force-stop")
	}

	st := h.slot.Status()
	if !st.Wedged {
		t.Error("status.Wedged = false, want true")
	}
	if st.ConsecutiveFailures != 1 {
		t.Errorf("failures = %d, want 1 — a wedge escalates the streak even after a clean job", st.ConsecutiveFailures)
	}

	rec := h.records(t)[0]
	if rec.Result != cycle.ResultFailure {
		t.Errorf("result = %s, want failure", rec.Result)
	}
	if rec.Ending != cycle.EndingWedge {
		t.Errorf("Ending = %q, want %q — the wedge outranks the success the cycle earned before teardown", rec.Ending, cycle.EndingWedge)
	}
	// The job really did run and complete — that is what makes this the
	// ok && wedged corner rather than a re-run of the failure-then-wedge test.
	if rec.Job == nil {
		t.Error("job not recorded; this test must exercise the success-then-wedge path")
	}
}

// Backoff arithmetic is one of the two riskiest untested paths: growth must
// be exponential from the base, capped, immune to shift overflow, and zero
// on a clean slate.
func TestBackoffProgression(t *testing.T) {
	h := newHarness(t, func(c *home.Config) {
		c.Limits.BackoffBase = home.Duration(time.Second)
		c.Limits.BackoffCap = home.Duration(30 * time.Second)
	})
	set := func(n uint32) {
		h.slot.mu.Lock()
		h.slot.failures = n
		h.slot.mu.Unlock()
	}
	cases := []struct {
		failures uint32
		want     time.Duration
	}{
		{0, 0},
		{1, time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{5, 16 * time.Second},
		{6, 30 * time.Second},   // capped
		{100, 30 * time.Second}, // shift saturates, never overflows
	}
	for _, tc := range cases {
		set(tc.failures)
		if got := h.slot.currentBackoff(); got != tc.want {
			t.Errorf("backoff(failures=%d) = %v, want %v", tc.failures, got, tc.want)
		}
	}
}

// Teardown hang-resistance: a wedged guest whose diag pull never returns on
// its own must not hold TEARDOWN past its budget — crash-only means teardown
// converges no matter what the guest does.
func TestTeardownBoundedDespiteWedgedDiagPull(t *testing.T) {
	h := newHarness(t, nil)
	h.guest.diagBlock = true // PullDiag blocks until its ctx expires
	h.vmF.machine.ip = ""    // fail at AWAIT_IP → failure teardown pulls diag
	cancel := h.start(t)
	_ = cancel

	h.waitState(t, StateTeardown)
	start := time.Now()
	h.waitState(t, StateBackoff)
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Errorf("teardown took %v with a wedged diag pull; its budget is 2s", elapsed)
	}
	// And the machine still got stopped despite the diag hang.
	h.vmF.machine.mu.Lock()
	stopped := h.vmF.machine.stopped
	h.vmF.machine.mu.Unlock()
	if !stopped {
		t.Error("teardown never stopped the machine")
	}
}

// The configured ref is config, not cycle state: it must be visible before
// Run ever starts (and immediately after a daemon restart).
func TestStatusCarriesImageFromConstruction(t *testing.T) {
	h := newHarness(t, nil)
	if got := h.slot.Status().Image; got != "ghcr.io/test/image:1" {
		t.Errorf("Status().Image = %q before Run, want the configured pool ref", got)
	}
	if got := h.slot.Status().ImageDigest; got != "" {
		t.Errorf("Status().ImageDigest = %q before any cycle, want empty", got)
	}
}

// A cycle that fails before Ensure returns must never expose a digest in
// live status — empty digest means "the registry was not reached this
// cycle". The record still carries the configured ref: intent is recorded
// at cycle start, so even a cycle that died mid-pull says what it was
// pulling.
func TestEnsureFailureExposesNoDigestButRecordsRef(t *testing.T) {
	h := newHarness(t, nil)
	h.images.err = errors.New("registry unreachable")
	cancel := h.start(t)
	_ = cancel

	st := h.waitState(t, StateTeardown)
	if st.ImageDigest != "" {
		t.Errorf("ImageDigest = %q after a failed pull, want empty", st.ImageDigest)
	}
	st = h.waitState(t, StateBackoff)
	if st.ImageDigest != "" {
		t.Errorf("ImageDigest = %q in BACKOFF after a failed pull, want empty", st.ImageDigest)
	}
	cancel()
	<-h.runDone

	recs := h.records(t)
	if len(recs) == 0 {
		t.Fatal("no cycle records written")
	}
	for _, rec := range recs {
		if rec.Image != "ghcr.io/test/image:1" {
			t.Errorf("record Image = %q, want the configured ref even when the pull failed", rec.Image)
		}
		if rec.ImageDigest != "" {
			t.Errorf("record ImageDigest = %q, want empty: nothing resolved", rec.ImageDigest)
		}
	}
}

// The runner tarball must be cloned per-cycle into the slot's OWN mount dir and
// THAT dir mounted into the guest — never the shared download store. This is the
// cache collapse: the cycle owns its tarball end to end, so no concurrent slot
// or store GC can disturb what the guest boots with.
func TestRunnerTarballClonedIntoPerSlotMount(t *testing.T) {
	h := newHarness(t, nil)
	h.start(t)

	// Drive to LISTENING so both CLONE and BOOT have run.
	h.proc.say(markerListening)
	h.waitState(t, StateListening)

	const tarball = "actions-runner-osx-arm64-2.320.0.tar.gz" // fakeImages' RunnerVersion
	mount := h.dir.SlotRunnerMountDir("runner-1")

	// BOOT mounted the per-slot dir, not the shared store.
	if got := h.vmF.lastRunnerCacheDir(); got != mount {
		t.Errorf("BOOT mounted %q, want the per-slot mount %q (not the shared store %q)",
			got, mount, h.dir.RunnerCacheDir())
	}

	// CLONE cloned the resolved tarball from the store into that mount, exactly once.
	calls := h.cloneFileCalls()
	want := [2]string{filepath.Join(h.dir.RunnerCacheDir(), tarball), filepath.Join(mount, tarball)}
	if len(calls) != 1 || calls[0] != want {
		t.Errorf("CloneFile calls = %v, want exactly [{%q %q}]", calls, want[0], want[1])
	}
}

// CLONE must self-heal a vms/<slot>/ left dirty by a prior cycle's best-effort
// teardown cleanup: it clears the dir before cloning, so a stale file can't make
// clonefile(2) fail EEXIST and wedge the slot in a CLONE→BACKOFF loop until a
// cold start.
func TestCloneClearsStaleVMDir(t *testing.T) {
	h := newHarness(t, nil)
	vmDir := h.dir.VMDir("runner-1")
	if err := os.MkdirAll(vmDir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(vmDir, "leftover-from-failed-teardown")
	if err := os.WriteFile(stale, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	h.start(t)
	h.proc.say(markerListening)
	h.waitState(t, StateListening) // reached LISTENING ⇒ the stale file didn't wedge CLONE

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("CLONE left a stale vms/<slot>/ file in place (stat err=%v); it must clear the dir before cloning", err)
	}
}

// ActiveCycleStates must accumulate completed states in the live snapshot and
// clear when the slot enters BACKOFF between cycles.
func TestActiveCycleStatesAccumulate(t *testing.T) {
	h := newHarness(t, nil)
	cancel := h.start(t)
	_ = cancel

	// ENSURE_IMAGE completes → CLONE enters; the snapshot at CLONE must carry
	// ENSURE_IMAGE as a completed state.
	cloneSt := h.waitState(t, StateClone)
	if n := len(cloneSt.ActiveCycleStates); n != 1 {
		t.Errorf("at CLONE: ActiveCycleStates has %d entries, want 1 (ENSURE_IMAGE)", n)
	} else if cloneSt.ActiveCycleStates[0].State != string(StateEnsureImage) {
		t.Errorf("at CLONE: ActiveCycleStates[0].State = %q, want ENSURE_IMAGE", cloneSt.ActiveCycleStates[0].State)
	} else if cloneSt.ActiveCycleStates[0].Left.IsZero() {
		t.Error("at CLONE: completed ENSURE_IMAGE record has zero Left timestamp")
	}

	// By LISTENING many states have completed; every completed state has
	// non-zero Left (set when the state finished running).
	h.proc.say(markerListening)
	listSt := h.waitState(t, StateListening)
	for _, sr := range listSt.ActiveCycleStates {
		if sr.Left.IsZero() {
			t.Errorf("ActiveCycleStates[%s].Left is zero — state was recorded incomplete", sr.State)
		}
	}
	// All states up to (but not including) LISTENING must be present, exactly
	// once each. Count is tied to the expected list so it doesn't need updating
	// when a new state is added — only the membership slice below does.
	wantBeforeListening := []State{StateEnsureImage, StateClone, StateBoot, StateAwaitIP, StateAwaitSSH, StateSecureSSH, StateMintJIT, StateProvision}
	stateNames := make(map[string]bool, len(listSt.ActiveCycleStates))
	for _, sr := range listSt.ActiveCycleStates {
		stateNames[sr.State] = true
	}
	if n := len(listSt.ActiveCycleStates); n != len(wantBeforeListening) {
		t.Errorf("at LISTENING: ActiveCycleStates has %d entries, want %d (one per pre-LISTENING state)", n, len(wantBeforeListening))
	}
	for _, want := range wantBeforeListening {
		if !stateNames[string(want)] {
			t.Errorf("ActiveCycleStates missing %s at LISTENING entry", want)
		}
	}

	// TEARDOWN snapshot must carry all states completed before teardown
	// (everything up through JOB). A regression in teardown's clone would
	// silently drop entries.
	h.proc.say("Running job: active-cycle-test")
	h.waitState(t, StateJob)
	h.proc.say("Job active-cycle-test completed with result: Succeeded")
	h.proc.exit(0)
	teardownSt := h.waitState(t, StateTeardown)
	if n := len(teardownSt.ActiveCycleStates); n == 0 {
		t.Error("TEARDOWN snapshot has empty ActiveCycleStates — completedBeforeTeardown was not set")
	}

	// BACKOFF must clear the slice entirely.
	backoffSt := h.waitState(t, StateBackoff)
	if len(backoffSt.ActiveCycleStates) != 0 {
		t.Errorf("BACKOFF snapshot has %d ActiveCycleStates, want 0", len(backoffSt.ActiveCycleStates))
	}
	cancel()
	<-h.runDone
}

func TestRunnerNameShape(t *testing.T) {
	h := newHarness(t, nil)
	cancel := h.start(t)
	_ = cancel
	h.waitState(t, StateProvision)
	h.gh.mu.Lock()
	var name string
	for n := range h.gh.registered {
		name = n
	}
	h.gh.mu.Unlock()
	cancel()
	// <prefix>-<slot>-<cycle8>
	parts := strings.Split(name, "-")
	if !strings.HasPrefix(name, "runny-runner-1-") || len(parts[len(parts)-1]) != 8 {
		t.Errorf("runner name = %q", name)
	}
}

// SECURE_SSH success must swap the cycle onto the rotated session: the
// runner launches over the new guest, never the password one, and the cycle
// record carries the state as OK.
func TestSecureSSHRotatesGuest(t *testing.T) {
	h := newHarness(t, nil)
	rotatedProc := newFakeProc()
	rotated := &fakeGuest{proc: rotatedProc}
	h.dialer.mu.Lock()
	h.dialer.rotated = rotated
	h.dialer.mu.Unlock()
	cancel := h.start(t)
	_ = cancel

	h.waitState(t, StateProvision)
	rotatedProc.say(markerListening)
	h.waitState(t, StateListening)
	rotatedProc.say("Running job: build")
	h.waitState(t, StateJob)
	rotatedProc.say("Job build completed with result: Succeeded")
	rotatedProc.exit(0)
	h.waitState(t, StateBackoff)
	cancel()
	<-h.runDone

	// The runner was staged over the rotated session, with the pool's OS.
	rotated.mu.Lock()
	rotatedGoos := rotated.goos
	rotated.mu.Unlock()
	if rotatedGoos != "darwin" {
		t.Errorf("rotated guest goos = %q; runner did not launch over the rotated session", rotatedGoos)
	}
	h.guest.mu.Lock()
	originalGoos := h.guest.goos
	h.guest.mu.Unlock()
	if originalGoos != "" {
		t.Error("runner launched over the password session despite rotation")
	}
	if got := h.dialer.rotations(); got < 1 {
		t.Errorf("rotations = %d, want >= 1", got)
	}

	var rec *cycle.Record
	for _, r := range h.records(t) {
		if r.Result == cycle.ResultSuccess {
			rec = r
		}
	}
	if rec == nil {
		t.Fatal("no success record")
	}
	var found bool
	for _, sr := range rec.States {
		if sr.State == string(StateSecureSSH) && sr.Outcome == cycle.OutcomeOK {
			found = true
		}
	}
	if !found {
		t.Errorf("no OK SECURE_SSH record in %+v", rec.States)
	}
}

// ssh_hardening off must skip the state entirely: no Rotate call, no
// SECURE_SSH record — the off-path cycle is byte-identical to the
// pre-rotation daemon.
func TestSecureSSHOffSkips(t *testing.T) {
	h := newHarnessPool(t, nil, func(p *home.PoolConfig) {
		p.SSHHardening = home.SSHHardeningOff
	})
	cancel := h.start(t)
	_ = cancel

	h.waitState(t, StateProvision)
	h.proc.say(markerListening)
	h.waitState(t, StateListening)
	h.slot.Command(Command{Kind: CmdRecycle, Reason: "test done"})
	h.waitState(t, StateBackoff)
	cancel()
	<-h.runDone

	if got := h.dialer.rotations(); got != 0 {
		t.Errorf("rotations = %d with hardening off", got)
	}
	for _, r := range h.records(t) {
		for _, sr := range r.States {
			if sr.State == string(StateSecureSSH) {
				t.Errorf("SECURE_SSH recorded despite hardening off: %+v", sr)
			}
		}
	}
}

// Rotation failure (image lacks sudo, sshd_config.d, systemd...) is a normal
// cycle failure: teardown, attributed to SECURE_SSH, with the post-mortem
// pulled over the still-open password session.
func TestSecureSSHFailureTearsDownAttributed(t *testing.T) {
	h := newHarness(t, nil)
	h.dialer.mu.Lock()
	h.dialer.rotateErr = errors.New("rotate: installing cycle key: exit 1: sudo: command not found")
	h.dialer.mu.Unlock()
	cancel := h.start(t)
	_ = cancel

	h.waitState(t, StateSecureSSH)
	h.waitState(t, StateTeardown)
	st := h.waitState(t, StateBackoff)
	if st.ConsecutiveFailures != 1 {
		t.Errorf("failures = %d, want 1", st.ConsecutiveFailures)
	}
	cancel()
	<-h.runDone

	// A gated second cycle may have started before cancel landed; find the
	// SECURE_SSH-attributed failure rather than assuming it is newest.
	var rec *cycle.Record
	for _, r := range h.records(t) {
		if r.Failure != nil && r.Failure.State == string(StateSecureSSH) {
			rec = r
		}
	}
	if rec == nil {
		t.Fatalf("no SECURE_SSH-attributed failure in %+v", h.records(t))
	}
	if !strings.Contains(rec.Failure.Error, "sudo: command not found") {
		t.Errorf("failure error = %q; the step name must survive into the record", rec.Failure.Error)
	}
	// The password session was still the cycle's guest; post-mortem rode it.
	if !h.guest.pulled {
		t.Error("post-mortem not pulled over the password session after rotation failure")
	}
}

// A guest that wedges mid-rotation is bounded by the state deadline and the
// record says deadline, not error — slow-vs-stuck must stay distinguishable.
func TestSecureSSHDeadlineBounds(t *testing.T) {
	h := newHarness(t, nil)
	h.dialer.mu.Lock()
	h.dialer.rotateBlock = true
	h.dialer.mu.Unlock()
	cancel := h.start(t)
	_ = cancel

	h.waitState(t, StateSecureSSH)
	h.waitState(t, StateTeardown)
	h.waitState(t, StateBackoff)
	cancel()
	<-h.runDone

	// Scan every record: a gated second cycle interrupted by cancel writes
	// an OutcomeError record that may sort newer than the deadline one.
	var found bool
	for _, rec := range h.records(t) {
		for _, sr := range rec.States {
			if sr.State == string(StateSecureSSH) && sr.Outcome == cycle.OutcomeDeadline {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("SECURE_SSH not recorded as deadline expiry: %+v", h.records(t))
	}
}

// ---- debug-key injection (issue #39) ---------------------------------------

// debugCmd issues a CmdDebugKey and returns the reply (with a timeout). It
// fills CycleID/SeenState from the slot's current status so the consent pins
// match by default; callers override fields via mutate.
func (h *harness) debugCmd(t *testing.T, mutate func(*Command)) DebugKeyReply {
	t.Helper()
	st := h.slot.Status()
	reply := make(chan DebugKeyReply, 1)
	cmd := Command{
		Kind:        CmdDebugKey,
		Reason:      "test",
		PubKey:      "ssh-ed25519 AAAAOPKEY op@host",
		Fingerprint: "SHA256:testfp",
		Comment:     "op@host",
		Hold:        time.Hour,
		CycleID:     st.CycleID,
		SeenState:   st.State,
		Expires:     time.Now().Add(time.Minute),
		Reply:       reply,
	}
	if mutate != nil {
		mutate(&cmd)
	}
	if !h.slot.Command(cmd) {
		t.Fatal("command buffer full")
	}
	select {
	case r := <-reply:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("no debug reply within 5s")
		return DebugKeyReply{}
	}
}

// reachListening boots the slot to LISTENING and returns the cancel func.
func (h *harness) reachListening(t *testing.T) context.CancelFunc {
	t.Helper()
	cancel := h.start(t)
	h.waitState(t, StateProvision)
	h.proc.say("Listening for Jobs")
	h.waitState(t, StateListening)
	return cancel
}

func TestDebugFreezeFromListening(t *testing.T) {
	h := newHarness(t, nil)
	h.images.maxCalls = 1
	h.guest.hostKeys = []string{"192.168.64.9 ssh-ed25519 AAAAHOST"}
	cancel := h.reachListening(t)
	defer cancel()

	r := h.debugCmd(t, nil)
	if r.Err != nil {
		t.Fatalf("freeze failed: %v", r.Err)
	}
	if r.Armed {
		t.Error("LISTENING freeze must not be Armed")
	}
	if r.HoldUntil.IsZero() {
		t.Error("LISTENING freeze reply missing HoldUntil")
	}
	if len(r.HostKeys) != 1 {
		t.Errorf("host keys not returned: %v", r.HostKeys)
	}
	st := h.waitState(t, StateDebug)
	if st.DebugHoldExpires.IsZero() {
		t.Error("DebugHoldExpires not set in DEBUG")
	}
	if h.guest.stops() != 1 {
		t.Errorf("StopRunner called %d times, want 1 (verified kill before install)", h.guest.stops())
	}
	if h.guest.installs() != 1 {
		t.Errorf("InstallAuthorizedKey called %d times, want 1", h.guest.installs())
	}
}

// TestDebugFreezeRecordsOperatorUID pins that a Command carrying the
// peer-cred-read operator identity lands on the write-ahead InjectedKeys
// entry appendPending writes before any guest byte.
func TestDebugFreezeRecordsOperatorUID(t *testing.T) {
	h := newHarness(t, nil)
	h.images.maxCalls = 1
	cancel := h.reachListening(t)
	defer cancel()

	uid := uint32(503)
	r := h.debugCmd(t, func(c *Command) { c.OperatorUID = &uid; c.OperatorUser = "bob" })
	if r.Err != nil {
		t.Fatalf("freeze failed: %v", r.Err)
	}
	h.waitState(t, StateDebug)
	h.slot.Command(Command{Kind: CmdRecycle, Reason: "done"})
	h.waitState(t, StateTeardown)
	h.waitState(t, StateBackoff)

	rec := h.jobRecord(t)
	if len(rec.InjectedKeys) != 1 {
		t.Fatalf("expected 1 injected key, got %+v", rec.InjectedKeys)
	}
	k := rec.InjectedKeys[0]
	if k.OperatorUID == nil || *k.OperatorUID != uid || k.OperatorUser != "bob" {
		t.Errorf("operator uid/user did not land on the pending entry: %+v", k)
	}
}

// TestDebugReArmRecordsOperatorUID pins the same requirement on debugReArm:
// when an operator re-runs `debug` for a key already confirmed installed,
// the FSM just extends the hold instead of reinstalling — that path builds
// its own InjectedKey separately from appendPending's, so it needs its own
// coverage.
func TestDebugReArmRecordsOperatorUID(t *testing.T) {
	h := newHarness(t, nil)
	h.images.maxCalls = 1
	cancel := h.reachListening(t)
	defer cancel()

	if r := h.debugCmd(t, nil); r.Err != nil {
		t.Fatalf("freeze: %v", r.Err)
	}
	h.waitState(t, StateDebug)

	uid := uint32(502)
	r := h.debugCmd(t, func(c *Command) { c.OperatorUID = &uid; c.OperatorUser = "alice" })
	if r.Err != nil {
		t.Fatalf("re-arm: %v", r.Err)
	}
	h.slot.Command(Command{Kind: CmdRecycle, Reason: "done"})
	h.waitState(t, StateTeardown)
	h.waitState(t, StateBackoff)

	rec := h.jobRecord(t)
	if len(rec.InjectedKeys) != 2 {
		t.Fatalf("expected 2 injected keys (freeze + re-arm), got %+v", rec.InjectedKeys)
	}
	k := rec.InjectedKeys[1]
	if k.Outcome != "re-armed" {
		t.Fatalf("expected the re-arm entry, got outcome %q", k.Outcome)
	}
	if k.OperatorUID == nil || *k.OperatorUID != uid || k.OperatorUser != "alice" {
		t.Errorf("operator uid/user did not land on the re-arm entry: %+v", k)
	}
}

// TestMidJobRefusedRecordsOperatorUID pins that midJobInject's raced-refusal
// entry (SeenState mismatch) carries the operator identity, matching the
// sibling appendPending/debugReArm sites — a refused attempt is exactly the
// kind of event worth attributing.
func TestMidJobRefusedRecordsOperatorUID(t *testing.T) {
	h := newHarness(t, func(c *home.Config) {
		c.Limits.MaxJobDuration = home.Duration(10 * time.Second)
	})
	h.images.maxCalls = 1
	cancel := h.start(t)
	defer cancel()
	h.waitState(t, StateProvision)
	h.proc.say("Listening for Jobs")
	h.waitState(t, StateListening)
	h.proc.say("Running job: build")
	h.waitState(t, StateJob)

	uid := uint32(504)
	r := h.debugCmd(t, func(c *Command) {
		c.SeenState = StateListening // operator saw LISTENING; a job started before it was serviced
		c.OperatorUID = &uid
		c.OperatorUser = "carol"
	})
	if r.Err == nil {
		t.Fatal("expected the raced command to be refused")
	}

	h.proc.say("Job build completed with result: Succeeded")
	h.proc.exit(0)
	h.waitState(t, StateTeardown)
	h.waitState(t, StateBackoff)

	rec := h.jobRecord(t)
	var refused *cycle.InjectedKey
	for i, k := range rec.InjectedKeys {
		if k.Outcome == "refused" {
			refused = &rec.InjectedKeys[i]
		}
	}
	if refused == nil {
		t.Fatalf("no refused entry recorded: %+v", rec.InjectedKeys)
	}
	if refused.OperatorUID == nil || *refused.OperatorUID != uid || refused.OperatorUser != "carol" {
		t.Errorf("operator uid/user did not land on the refused entry: %+v", refused)
	}
}

// TestMidJobReArmRecordsOperatorUID is the mid-job version of
// TestDebugReArmRecordsOperatorUID: an operator re-running `debug` mid-job
// for a key already confirmed installed just extends the hold, without
// reinstalling — that entry must carry the operator identity too.
func TestMidJobReArmRecordsOperatorUID(t *testing.T) {
	h := newHarness(t, func(c *home.Config) {
		c.Limits.MaxJobDuration = home.Duration(10 * time.Second)
	})
	h.images.maxCalls = 1
	cancel := h.reachJobArmed(t)
	defer cancel()

	uid := uint32(505)
	r := h.debugCmd(t, func(c *Command) { c.OperatorUID = &uid; c.OperatorUser = "dave" })
	if r.Err != nil {
		t.Fatalf("mid-job re-arm: %v", r.Err)
	}

	h.proc.say("Job build completed with result: Succeeded")
	h.proc.exit(0)
	h.waitState(t, StateDebug)
	h.slot.Command(Command{Kind: CmdRecycle, Reason: "done"})
	h.waitState(t, StateTeardown)
	h.waitState(t, StateBackoff)

	rec := h.jobRecord(t)
	var rearmed *cycle.InjectedKey
	for i, k := range rec.InjectedKeys {
		if k.Outcome == "re-armed" {
			rearmed = &rec.InjectedKeys[i]
		}
	}
	if rearmed == nil {
		t.Fatalf("no re-armed entry recorded: %+v", rec.InjectedKeys)
	}
	if rearmed.OperatorUID == nil || *rearmed.OperatorUID != uid || rearmed.OperatorUser != "dave" {
		t.Errorf("operator uid/user did not land on the mid-job re-arm entry: %+v", rearmed)
	}
}

// TestMidJobDisarmHasNoOperator pins that auditDisarm entries carry no
// operator identity even when the arming Command did — they record the FSM
// disarming its OWN hold, not an operator act.
func TestMidJobDisarmHasNoOperator(t *testing.T) {
	h := newHarness(t, func(c *home.Config) {
		c.Limits.MaxJobDuration = home.Duration(10 * time.Second)
	})
	h.images.maxCalls = 1
	cancel := h.start(t)
	defer cancel()
	h.waitState(t, StateProvision)
	h.proc.say("Listening for Jobs")
	h.waitState(t, StateListening)
	h.proc.say("Running job: build")
	h.waitState(t, StateJob)

	uid := uint32(501)
	h.debugCmd(t, func(c *Command) { c.OperatorUID = &uid; c.OperatorUser = "brajkovic" })
	if !h.slot.Status().DebugHoldArmed {
		t.Fatal("precondition: armed")
	}

	h.slot.Command(Command{Kind: CmdRecycle, Reason: "no force"})
	h.proc.say("Job build completed with result: Succeeded")
	h.proc.exit(0)
	h.waitState(t, StateTeardown)
	h.waitState(t, StateBackoff)

	rec := h.jobRecord(t)
	var disarmed *cycle.InjectedKey
	for i, k := range rec.InjectedKeys {
		if k.Outcome == "disarmed" {
			disarmed = &rec.InjectedKeys[i]
		}
	}
	if disarmed == nil {
		t.Fatalf("no disarmed entry recorded: %+v", rec.InjectedKeys)
	}
	if disarmed.OperatorUID != nil {
		t.Errorf("disarmed entry must carry no operator, got uid %v", *disarmed.OperatorUID)
	}
}

func TestDebugFreezeKillUnprovenTearsDown(t *testing.T) {
	h := newHarness(t, nil)
	h.images.maxCalls = 1
	h.guest.stopErr = errors.New("pgrep failed")
	cancel := h.reachListening(t)
	defer cancel()

	r := h.debugCmd(t, nil)
	if r.Err == nil {
		t.Fatal("want error when kill is unproven")
	}
	// Never install when the kill is unproven.
	if h.guest.installs() != 0 {
		t.Errorf("installed despite unproven kill: %d", h.guest.installs())
	}
	h.waitState(t, StateTeardown)
}

func TestDebugRaceMarkerRefusesAndRunsJob(t *testing.T) {
	// A job marker that arrives just before the freeze is serviced must leave
	// the job untouched: either the LISTENING select reads the marker first
	// (clean JOB transition, freeze never runs) or the freeze's drain-check
	// catches it (refusal). Both are correct; both leave the guest untouched.
	// The select between proc.Lines() and s.cmds is random, so this asserts
	// the invariant common to both branches.
	h := newHarness(t, func(c *home.Config) {
		c.Limits.MaxJobDuration = home.Duration(10 * time.Second)
	})
	h.images.maxCalls = 1
	cancel := h.reachListening(t)
	defer cancel()

	h.proc.say("Running job: build")
	time.Sleep(50 * time.Millisecond)
	r := h.debugCmd(t, nil)
	// The job is never frozen or killed by a raced LISTENING command.
	if h.guest.stops() != 0 {
		t.Errorf("a raced marker killed the runner (StopRunner called %d)", h.guest.stops())
	}
	if r.Err != nil {
		// Drain-check refusal: nothing was injected.
		if !strings.Contains(r.Err.Error(), "re-run debug") {
			t.Errorf("freeze drain-check refusal had the wrong message: %v", r.Err)
		}
		if h.guest.installs() != 0 {
			t.Error("a refused freeze must not install")
		}
	}
	// Either way the slot ends up running the job.
	h.waitState(t, StateJob)
}

func TestDebugRacedKillInPostKillDrainIsBenignNoDeleteRunner(t *testing.T) {
	// Freeze step 4 (§5.1): the step-2 drain-check found no marker, so the
	// freeze proceeded to the verified kill — and a job raced in during the
	// kill, surfacing as a marker in the post-kill drain-to-close. That runner
	// picked up work and was killed, so GitHub considers it BUSY. The cycle
	// fails with errDebugRacedJob (benign), and the load-bearing invariant is
	// that jobRan=true: teardown must NOT call DeleteRunner against the
	// GitHub-busy runner (the coupling decision 2 rejected). This pins the
	// blocker-3 fix; without it the raced-kill path returned jobRan=false and
	// deregistered a busy runner.
	h := newHarness(t, nil)
	h.images.maxCalls = 1
	// The proven kill buffers a "raced" job marker right before closing Lines,
	// so the step-4 drain (which runs AFTER step-2 saw nothing) catches it.
	h.guest.stopSayMarker = "Running job: raced"
	cancel := h.reachListening(t)
	defer cancel()

	r := h.debugCmd(t, nil)
	if r.Err == nil || !strings.Contains(r.Err.Error(), "killed by the freeze") {
		t.Fatalf("want the raced-kill refusal, got %v", r.Err)
	}
	// The kill ran (step 3) but the install never did (the race aborted before
	// step 5).
	if h.guest.stops() != 1 {
		t.Errorf("StopRunner calls = %d, want 1", h.guest.stops())
	}
	if h.guest.installs() != 0 {
		t.Error("a raced-kill freeze must not install the key")
	}
	h.waitState(t, StateTeardown)
	bk := h.waitState(t, StateBackoff)
	// errDebugRacedJob is benign (operator-caused): the streak does not move.
	if bk.ConsecutiveFailures != 0 {
		t.Errorf("a raced-kill cycle is benign; failures=%d", bk.ConsecutiveFailures)
	}
	// The load-bearing assertion: jobRan=true, so teardown's !jobRan gate keeps
	// DeleteRunner from firing against a GitHub-busy runner.
	if len(h.gh.deleted) != 0 {
		t.Errorf("DeleteRunner called on a raced-kill cycle (%v); the runner is GitHub-busy and must not be deregistered", h.gh.deleted)
	}
}

func TestDebugExpiredCommandRejected(t *testing.T) {
	h := newHarness(t, nil)
	h.images.maxCalls = 1
	cancel := h.reachListening(t)
	defer cancel()

	r := h.debugCmd(t, func(c *Command) { c.Expires = time.Now().Add(-time.Second) })
	if r.Err == nil || !strings.Contains(r.Err.Error(), "expired") {
		t.Fatalf("want expired rejection, got %v", r.Err)
	}
	if h.guest.installs() != 0 {
		t.Error("an expired command must not touch the guest")
	}
}

func TestDebugStaleCycleRejected(t *testing.T) {
	h := newHarness(t, nil)
	h.images.maxCalls = 1
	cancel := h.reachListening(t)
	defer cancel()

	r := h.debugCmd(t, func(c *Command) { c.CycleID = "deadbeef" })
	if r.Err == nil || !strings.Contains(r.Err.Error(), "already ended") {
		t.Fatalf("want stale-cycle rejection, got %v", r.Err)
	}
}

func TestMidJobInjectArmsHold(t *testing.T) {
	h := newHarness(t, func(c *home.Config) {
		c.Limits.MaxJobDuration = home.Duration(10 * time.Second)
	})
	h.images.maxCalls = 1
	cancel := h.start(t)
	defer cancel()
	h.waitState(t, StateProvision)
	h.proc.say("Listening for Jobs")
	h.waitState(t, StateListening)
	h.proc.say("Running job: build")
	h.waitState(t, StateJob)

	r := h.debugCmd(t, nil)
	if r.Err != nil {
		t.Fatalf("mid-job inject failed: %v", r.Err)
	}
	if !r.Armed {
		t.Error("mid-job inject must be Armed")
	}
	if !r.HoldUntil.IsZero() {
		t.Error("armed reply must have zero HoldUntil (clock starts at job end)")
	}
	// The runner is NOT touched mid-job.
	if h.guest.stops() != 0 {
		t.Errorf("StopRunner called during JOB (%d); the job must be untouched", h.guest.stops())
	}
	if h.guest.installs() != 1 {
		t.Errorf("InstallAuthorizedKey called %d times, want 1", h.guest.installs())
	}
	// DebugHoldArmed visible in status during the job.
	if !h.slot.Status().DebugHoldArmed {
		t.Error("DebugHoldArmed not set after a mid-job arm")
	}
	// Job completes → DEBUG, hold clock starts at entry.
	h.proc.say("Job build completed with result: Succeeded")
	h.proc.exit(0)
	st := h.waitState(t, StateDebug)
	if st.DebugHoldArmed {
		t.Error("DebugHoldArmed not cleared at DEBUG entry")
	}
	if st.DebugHoldExpires.IsZero() {
		t.Error("DebugHoldExpires not set at DEBUG entry")
	}
	if h.guest.stops() != 1 {
		t.Errorf("post-job StopRunner not called once: %d", h.guest.stops())
	}
	// The job's operator key is on the record.
	recJob := h.slot.Status().Job
	if recJob == nil || len(recJob.OperatorKeys) != 1 {
		t.Errorf("job operator keys = %+v", recJob)
	}
}

func TestMidJobPlainRecycleDisarmsImmediately(t *testing.T) {
	h := newHarness(t, func(c *home.Config) {
		c.Limits.MaxJobDuration = home.Duration(10 * time.Second)
	})
	h.images.maxCalls = 1
	cancel := h.start(t)
	defer cancel()
	h.waitState(t, StateProvision)
	h.proc.say("Listening for Jobs")
	h.waitState(t, StateListening)
	h.proc.say("Running job: build")
	h.waitState(t, StateJob)
	h.debugCmd(t, nil)
	if !h.slot.Status().DebugHoldArmed {
		t.Fatal("precondition: armed")
	}

	// Plain recycle (no CancelJob): disarms immediately, job survives.
	h.slot.Command(Command{Kind: CmdRecycle, Reason: "no force"})
	deadline := time.After(2 * time.Second)
	for h.slot.Status().DebugHoldArmed {
		select {
		case <-deadline:
			t.Fatal("DebugHoldArmed not cleared after a plain recycle")
		case <-time.After(10 * time.Millisecond):
		}
	}
	// The job still finishes; the slot tears down at job end (NOT into DEBUG).
	h.proc.say("Job build completed with result: Succeeded")
	h.proc.exit(0)
	h.waitState(t, StateTeardown)
}

func TestMidJobCancelJobKillsJob(t *testing.T) {
	h := newHarness(t, func(c *home.Config) {
		c.Limits.MaxJobDuration = home.Duration(10 * time.Second)
	})
	h.images.maxCalls = 1
	cancel := h.start(t)
	defer cancel()
	h.waitState(t, StateProvision)
	h.proc.say("Listening for Jobs")
	h.waitState(t, StateListening)
	h.proc.say("Running job: build")
	h.waitState(t, StateJob)
	h.debugCmd(t, nil)

	h.slot.Command(Command{Kind: CmdRecycle, Reason: "force", CancelJob: true})
	h.waitState(t, StateTeardown)
	// jobRan=true → no DeleteRunner against a GitHub-busy runner.
	if len(h.gh.deleted) != 0 {
		t.Errorf("DeleteRunner called on a canceled mid-job recycle: %v", h.gh.deleted)
	}
}

func TestMidJobInstallFailureDoesNotArm(t *testing.T) {
	h := newHarness(t, func(c *home.Config) {
		c.Limits.MaxJobDuration = home.Duration(10 * time.Second)
	})
	h.images.maxCalls = 1
	h.guest.installErr = errors.New("ambiguous failure")
	cancel := h.start(t)
	defer cancel()
	h.waitState(t, StateProvision)
	h.proc.say("Listening for Jobs")
	h.waitState(t, StateListening)
	h.proc.say("Running job: build")
	h.waitState(t, StateJob)

	r := h.debugCmd(t, nil)
	if r.Err == nil {
		t.Fatal("want error on a failed mid-job install")
	}
	if h.slot.Status().DebugHoldArmed {
		t.Error("a failed install must not arm")
	}
	// The job continues and completes → teardown (not DEBUG).
	h.proc.say("Job build completed with result: Succeeded")
	h.proc.exit(0)
	h.waitState(t, StateTeardown)
}

func TestMidJobRetryAfterAmbiguousErrorReinstalls(t *testing.T) {
	h := newHarness(t, func(c *home.Config) {
		c.Limits.MaxJobDuration = home.Duration(10 * time.Second)
	})
	h.images.maxCalls = 1
	// First install fails ambiguously; the second succeeds.
	h.guest.installErrSeq = []error{errors.New("ambiguous"), nil}
	cancel := h.start(t)
	defer cancel()
	h.waitState(t, StateProvision)
	h.proc.say("Listening for Jobs")
	h.waitState(t, StateListening)
	h.proc.say("Running job: build")
	h.waitState(t, StateJob)

	if r := h.debugCmd(t, nil); r.Err == nil {
		t.Fatal("first attempt should fail")
	}
	// Retry of the SAME fingerprint after an ambiguous error must run the full
	// install again (not the exec-free re-arm).
	r := h.debugCmd(t, nil)
	if r.Err != nil {
		t.Fatalf("retry failed: %v", r.Err)
	}
	if !r.Armed {
		t.Error("retry should arm after a proven install")
	}
	if h.guest.installs() != 2 {
		t.Errorf("retry did not re-run install: installs=%d, want 2", h.guest.installs())
	}
}

func TestMidJobSeenStateMismatchRefused(t *testing.T) {
	h := newHarness(t, func(c *home.Config) {
		c.Limits.MaxJobDuration = home.Duration(10 * time.Second)
	})
	h.images.maxCalls = 1
	cancel := h.start(t)
	defer cancel()
	h.waitState(t, StateProvision)
	h.proc.say("Listening for Jobs")
	h.waitState(t, StateListening)
	h.proc.say("Running job: build")
	h.waitState(t, StateJob)

	// Operator saw LISTENING but the command is dequeued in JOB.
	r := h.debugCmd(t, func(c *Command) { c.SeenState = StateListening })
	if r.Err == nil || !strings.Contains(r.Err.Error(), "re-run debug") {
		t.Fatalf("want consent refusal, got %v", r.Err)
	}
	if h.guest.installs() != 0 {
		t.Error("a SeenState mismatch must not touch the guest")
	}
}

func TestMidJobGuestUnreachableNoRedial(t *testing.T) {
	h := newHarness(t, func(c *home.Config) {
		c.Limits.MaxJobDuration = home.Duration(10 * time.Second)
	})
	h.images.maxCalls = 1
	h.guest.installErr = fmt.Errorf("session: %w", ErrGuestUnreachable)
	cancel := h.start(t)
	defer cancel()
	h.waitState(t, StateProvision)
	h.proc.say("Listening for Jobs")
	h.waitState(t, StateListening)
	h.proc.say("Running job: build")
	h.waitState(t, StateJob)

	r := h.debugCmd(t, nil)
	if r.Err == nil {
		t.Fatal("want unreachable error")
	}
	if h.guest.redials() != 0 {
		t.Errorf("Redial called mid-job (%d); decision 18 forbids it", h.guest.redials())
	}
}

func TestDebugRecycleFromDebugIsBenign(t *testing.T) {
	h := newHarness(t, nil)
	h.images.maxCalls = 1
	cancel := h.reachListening(t)
	defer cancel()

	if r := h.debugCmd(t, nil); r.Err != nil {
		t.Fatalf("freeze: %v", r.Err)
	}
	h.waitState(t, StateDebug)
	h.slot.Command(Command{Kind: CmdRecycle, Reason: "done debugging"})
	h.waitState(t, StateTeardown)
	st := h.waitState(t, StateBackoff)
	// A LISTENING freeze with a brief hold and a benign exit: the streak does
	// not increment (heldListening is false for <10min, but errDebugExpired /
	// recycle are benign).
	if st.ConsecutiveFailures != 0 {
		t.Errorf("recycle from DEBUG should be benign; failures=%d", st.ConsecutiveFailures)
	}
}

func TestDebugReArmInDebugNoExec(t *testing.T) {
	h := newHarness(t, nil)
	h.images.maxCalls = 1
	cancel := h.reachListening(t)
	defer cancel()

	if r := h.debugCmd(t, nil); r.Err != nil {
		t.Fatalf("freeze: %v", r.Err)
	}
	h.waitState(t, StateDebug)
	installsBefore := h.guest.installs()
	// Re-arm with the SAME fingerprint: no guest exec.
	r := h.debugCmd(t, nil)
	if r.Err != nil {
		t.Fatalf("re-arm: %v", r.Err)
	}
	if h.guest.installs() != installsBefore {
		t.Errorf("re-arm of an installed key ran an exec: installs %d→%d", installsBefore, h.guest.installs())
	}
}

func TestPostJobKillUnprovenCounts(t *testing.T) {
	h := newHarness(t, func(c *home.Config) {
		c.Limits.MaxJobDuration = home.Duration(10 * time.Second)
	})
	h.images.maxCalls = 1
	cancel := h.start(t)
	defer cancel()
	h.waitState(t, StateProvision)
	h.proc.say("Listening for Jobs")
	h.waitState(t, StateListening)
	h.proc.say("Running job: build")
	h.waitState(t, StateJob)
	if r := h.debugCmd(t, nil); r.Err != nil {
		t.Fatalf("arm: %v", r.Err)
	}
	// The post-job kill fails: no DEBUG StateRecord, errDebugInjectFailed.
	h.guest.stopErr = errors.New("post-job pgrep failed")
	h.proc.say("Job build completed with result: Succeeded")
	h.proc.exit(0)
	h.waitState(t, StateTeardown)
	st := h.waitState(t, StateBackoff)
	if st.ConsecutiveFailures != 1 {
		t.Errorf("post-job kill unproven should count; failures=%d", st.ConsecutiveFailures)
	}
}

// reachJobArmed boots the slot to JOB, arms a mid-job hold, and returns the
// cancel func. MaxJobDuration is left to the caller's mutate.
func (h *harness) reachJobArmed(t *testing.T) context.CancelFunc {
	t.Helper()
	cancel := h.start(t)
	h.waitState(t, StateProvision)
	h.proc.say("Listening for Jobs")
	h.waitState(t, StateListening)
	h.proc.say("Running job: build")
	h.waitState(t, StateJob)
	if r := h.debugCmd(t, nil); r.Err != nil {
		t.Fatalf("arm: %v", r.Err)
	}
	if !h.slot.Status().DebugHoldArmed {
		t.Fatal("precondition: armed")
	}
	return cancel
}

// jobRecord returns the single completed cycle record (the harness runs one
// cycle then gates the next on backoff).
func (h *harness) jobRecord(t *testing.T) *cycle.Record {
	t.Helper()
	recs := h.records(t)
	if len(recs) == 0 {
		t.Fatal("no cycle record written")
	}
	return recs[0]
}

func TestMidJobBudgetExpiryArmsEntersDebug(t *testing.T) {
	// A budget-expired-but-armed job: the runner is killed (now legitimate —
	// the FSM already condemned the job), the slot enters DEBUG holding the
	// corpse, the cycle records the JOB budget failure, and the streak counts
	// (decision 17/20, §8 row "Job failed / budget expired → hold").
	h := newHarness(t, func(c *home.Config) {
		c.Limits.MaxJobDuration = home.Duration(300 * time.Millisecond)
	})
	h.images.maxCalls = 1
	cancel := h.start(t)
	defer cancel()
	h.waitState(t, StateProvision)
	h.proc.say("Listening for Jobs")
	h.waitState(t, StateListening)
	h.proc.say("Running job: build")
	h.waitState(t, StateJob)
	if r := h.debugCmd(t, nil); r.Err != nil {
		t.Fatalf("arm: %v", r.Err)
	}
	// Say nothing further: the job neither completes nor exits, so jctx fires
	// the budget and drainForCompletion finds no completion marker.
	st := h.waitState(t, StateDebug)
	if st.DebugHoldExpires.IsZero() {
		t.Error("DebugHoldExpires not set after a budget-expiry hold entry")
	}
	// The post-job kill ran (the listener was alive at budget expiry).
	if h.guest.stops() != 1 {
		t.Errorf("post-job StopRunner calls = %d, want 1", h.guest.stops())
	}
	// Release the hold so the cycle finishes and records.
	h.slot.Command(Command{Kind: CmdRecycle, Reason: "release"})
	h.waitState(t, StateTeardown)
	bk := h.waitState(t, StateBackoff)
	// The job's budget failure owns the cycle: streak++ (the hold does not
	// launder it).
	if bk.ConsecutiveFailures != 1 {
		t.Errorf("budget-expired armed cycle should count; failures=%d", bk.ConsecutiveFailures)
	}
	rec := h.jobRecord(t)
	if rec.Failure == nil || rec.Failure.State != string(StateJob) {
		t.Errorf("cycle failure = %+v, want State=JOB (the budget error owns the cycle)", rec.Failure)
	}
	if !strings.Contains(rec.Failure.Error, "budget") {
		t.Errorf("failure error = %q, want a budget message", rec.Failure.Error)
	}
}

func TestDrainForCompletionClassifiesCompletedNotBudget(t *testing.T) {
	// The §3 coin-flip fix, pinned deterministically: when the budget fires
	// (jctx.Done()) in the SAME window a completion signal is already buffered
	// in proc.Lines(), drainForCompletion must classify the job as COMPLETED,
	// never a budget blowout. A blind budget interval would coin-flip this; the
	// drain-check makes the accounting exact.
	s := NewSlot("t", Deps{})

	t.Run("buffered completion marker → completed/ok", func(t *testing.T) {
		p := newFakeProc()
		p.say("Job build completed with result: Succeeded")
		done, ok := s.drainForCompletion(p, "cyc")
		if !done || !ok {
			t.Errorf("drainForCompletion = (%v, %v), want (true, true) — a buffered marker is a completion", done, ok)
		}
	})

	t.Run("channel closed with code 0 → completed/ok", func(t *testing.T) {
		p := newFakeProc()
		p.exit(0)
		done, ok := s.drainForCompletion(p, "cyc")
		if !done || !ok {
			t.Errorf("drainForCompletion = (%v, %v), want (true, true)", done, ok)
		}
	})

	t.Run("channel closed with nonzero code → completed/not-ok", func(t *testing.T) {
		p := newFakeProc()
		p.exit(7)
		done, ok := s.drainForCompletion(p, "cyc")
		if !done || ok {
			t.Errorf("drainForCompletion = (%v, %v), want (true, false)", done, ok)
		}
	})

	t.Run("nothing buffered → genuine blowout", func(t *testing.T) {
		p := newFakeProc()
		p.say("still working, no completion in sight")
		done, ok := s.drainForCompletion(p, "cyc")
		if done || ok {
			t.Errorf("drainForCompletion = (%v, %v), want (false, false) — a real budget blowout", done, ok)
		}
	})
}

func TestPostJobDrainTimeoutForceClosesNoWait(t *testing.T) {
	// The FSM-hang fix (§5.4 step 2): when the post-job drain does not close
	// within its bound (an orphaned job descendant holds the inherited stdout
	// fd), enterPostJobDebug force-closes the channel and enters DEBUG — and it
	// must NOT call proc.Wait(), which would block the FSM goroutine up to
	// max_debug_hold on exactly that pathology (an unbounded-operation violation).
	h := newHarness(t, func(c *home.Config) {
		c.Limits.MaxJobDuration = home.Duration(10 * time.Second)
	})
	h.images.maxCalls = 1
	// The proven post-job kill leaves Lines OPEN (orphaned-fd pathology), so
	// drainToClose times out at 5s and force-closes without Wait.
	h.guest.stopNoClose = true
	cancel := h.reachJobArmed(t)
	defer cancel()

	// A completion marker ends the job; the post-job tail then kills (proven,
	// but Lines stays open), drains-to-timeout (a fixed 5s in enterPostJobDebug),
	// force-closes, and enters DEBUG. The 5s drain bound is longer than
	// waitState's deadline, so poll the slot directly.
	h.proc.say("Job build completed with result: Succeeded")
	deadline := time.After(8 * time.Second)
	for h.slot.Status().State != StateDebug {
		select {
		case <-deadline:
			t.Fatalf("never reached DEBUG after the force-close drain (currently %s)", h.slot.Status().State)
		case <-time.After(20 * time.Millisecond):
		}
	}
	if h.slot.Status().DebugHoldExpires.IsZero() {
		t.Error("DEBUG entered but DebugHoldExpires unset")
	}
	if h.guest.stops() != 1 {
		t.Errorf("post-job StopRunner calls = %d, want 1", h.guest.stops())
	}
	// The load-bearing assertion: the force-close path never harvested Wait.
	if h.proc.waits() != 0 {
		t.Errorf("proc.Wait() called %d times on the force-close path; the FSM-hang fix requires zero", h.proc.waits())
	}
	// Release the hold explicitly → benign teardown (the default 1h hold would
	// otherwise outlive the test).
	h.slot.Command(Command{Kind: CmdRecycle, Reason: "release"})
	h.waitState(t, StateTeardown)
}

func TestDebugHeldAfterJobOKResetsStreak(t *testing.T) {
	// §8: a DELIVERED job followed by a benign hold resets the streak via
	// debugHeldAfterJobOK — distinct from the heldListening (≥10-min idle)
	// path. The fixture has NO ≥10-min LISTENING record, so the reset can only
	// come from the JOB-OK + DEBUG-record carve-out.
	h := newHarness(t, func(c *home.Config) {
		c.Limits.MaxJobDuration = home.Duration(10 * time.Second)
	})
	h.images.maxCalls = 1
	// Seed a failing streak so a reset is observable.
	h.slot.mu.Lock()
	h.slot.failures = 3
	h.slot.mu.Unlock()
	cancel := h.reachJobArmed(t)
	defer cancel()

	// Job completes → armed → DEBUG. The hold then expires benignly.
	h.proc.say("Job build completed with result: Succeeded")
	h.proc.exit(0)
	h.waitState(t, StateDebug)
	h.slot.Command(Command{Kind: CmdRecycle, Reason: "done"})
	h.waitState(t, StateTeardown)
	bk := h.waitState(t, StateBackoff)
	if bk.ConsecutiveFailures != 0 {
		t.Errorf("delivered job + benign hold must reset the streak; failures=%d", bk.ConsecutiveFailures)
	}
	rec := h.jobRecord(t)
	if !debugHeldAfterJobOK(rec) {
		t.Error("debugHeldAfterJobOK(rec) = false; the reset must come from the JOB-OK + DEBUG carve-out")
	}
	if heldListening(rec) {
		t.Error("fixture unexpectedly has a ≥10-min LISTENING record; the reset must be debugHeldAfterJobOK, not heldListening")
	}
}

// --- debug session recording ---

// TestStripTerminalCodes: ANSI sequences and CR+LF are removed; plain text is
// preserved.
func TestStripTerminalCodes(t *testing.T) {
	tests := []struct{ in, want string }{
		{"\x1b[31mred\x1b[0m", "red"},                 // SGR color
		{"\x1b[2J", ""},                               // CSI erase-screen
		{"\x1b[?25h", ""},                             // CSI private mode
		{"\x1b[3~", ""},                               // CSI ~ terminator (Delete key)
		{"\x1bM", ""},                                 // 2-char standalone ESC sequence
		{"\x1b(B", ""},                                // 3-char designator: US-ASCII (vim)
		{"\x1b(0", ""},                                // 3-char designator: DEC line drawing (ncurses)
		{"hello\r\nworld\r\n", "hello\nworld\n"},      // CRLF: \r stripped, \n preserved
		{"\x1b[32mok\x1b[0m\r\n", "ok\n"},             // ANSI stripped + CRLF normalized
		{"plain text\n", "plain text\n"},              // untouched
		{"\x1b]0;My Title\x07prompt$ ", "prompt$ "},   // OSC BEL-terminated (macOS Terminal)
		{"\x1b]0;My Title\x1b\\prompt$ ", "prompt$ "}, // OSC ST-terminated
		{"overwrite\rprogress", "overwriteprogress"},  // bare \r (curl/npm progress bars)
	}
	for _, tc := range tests {
		got := string(stripTerminalCodes([]byte(tc.in)))
		if got != tc.want {
			t.Errorf("stripTerminalCodes(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestDebugSessionPulledWhenKeyLanded: a cycle that entered DEBUG via a landed
// key and has a non-empty session log produces a debug-session.log artifact.
func TestDebugSessionPulledWhenKeyLanded(t *testing.T) {
	h := newHarness(t, nil)
	h.images.maxCalls = 1
	// Return raw terminal output; the artifact on disk must have codes stripped.
	h.guest.sessionLog = []byte("\x1b[32moperator ran some commands\x1b[0m\r\n")
	cancel := h.reachListening(t)
	defer cancel()

	r := h.debugCmd(t, nil)
	if r.Err != nil {
		t.Fatalf("freeze failed: %v", r.Err)
	}
	h.waitState(t, StateDebug)
	h.slot.Command(Command{Kind: CmdRecycle, Reason: "done"})
	h.waitState(t, StateTeardown)
	h.waitState(t, StateBackoff)

	rec := h.jobRecord(t)
	if !slices.Contains(rec.Artifacts, "debug-session.log") {
		t.Errorf("debug-session.log missing from artifacts: %v", rec.Artifacts)
	}
	if !h.guest.sessionPulledOnce() {
		t.Error("PullDebugSession was not called")
	}
	dir, _ := (cycle.Store{SlotDir: h.dir.SlotCyclesDir("runner-1")}).Dir(rec)
	got, err := os.ReadFile(filepath.Join(dir, "debug-session.log"))
	if err != nil {
		t.Fatalf("reading debug-session.log: %v", err)
	}
	if want := "operator ran some commands\n"; string(got) != want {
		t.Errorf("debug-session.log content = %q, want %q (ANSI codes not stripped?)", got, want)
	}
}

// TestDebugSessionSkippedWhenEmpty: an empty session log (operator never
// connected) must not produce a debug-session.log artifact.
func TestDebugSessionSkippedWhenEmpty(t *testing.T) {
	h := newHarness(t, nil)
	h.images.maxCalls = 1
	// sessionLog is nil (the zero value)
	cancel := h.reachListening(t)
	defer cancel()

	r := h.debugCmd(t, nil)
	if r.Err != nil {
		t.Fatalf("freeze failed: %v", r.Err)
	}
	h.waitState(t, StateDebug)
	h.slot.Command(Command{Kind: CmdRecycle, Reason: "done"})
	h.waitState(t, StateTeardown)
	h.waitState(t, StateBackoff)

	rec := h.jobRecord(t)
	if slices.Contains(rec.Artifacts, "debug-session.log") {
		t.Errorf("debug-session.log artifact must be skipped on empty session log; artifacts: %v", rec.Artifacts)
	}
}

// TestDebugSessionNotPulledWithoutLandedKey: a normal successful cycle (no
// debug key injection) must not call PullDebugSession.
func TestDebugSessionNotPulledWithoutLandedKey(t *testing.T) {
	h := newHarness(t, nil)
	h.images.maxCalls = 1
	cancel := h.reachListening(t)
	defer cancel()

	h.proc.say("Running job: build")
	h.proc.say("build completed with result: Succeeded")
	h.proc.exit(0)
	h.waitState(t, StateBackoff)

	if h.guest.sessionPulledOnce() {
		t.Error("PullDebugSession must not be called when no debug key landed")
	}
}

// --- streak-accounting carve-out unit tests (decisions 18, 20, §8) ---
// These pin the decided semantics with hand-built records: the runtime paths
// above exercise the wiring, these pin the classification functions directly
// (the ≥10-min LISTENING window is impractical to produce at test runtime).

func jobRec(outcome cycle.Outcome) cycle.StateRecord {
	return cycle.StateRecord{State: string(StateJob), Entered: time.Now().Add(-time.Minute), Left: time.Now(), Outcome: outcome}
}

func longListeningRec() cycle.StateRecord {
	return cycle.StateRecord{State: string(StateListening), Entered: time.Now().Add(-11 * time.Minute), Left: time.Now(), Outcome: cycle.OutcomeOK}
}

func debugRec() cycle.StateRecord {
	return cycle.StateRecord{State: string(StateDebug), Entered: time.Now().Add(-time.Minute), Left: time.Now(), Outcome: cycle.OutcomeOK}
}

func TestStreakCarveOuts(t *testing.T) {
	listeningErr := cycle.InjectedKey{Outcome: "error", State: string(StateListening)}
	jobErr := cycle.InjectedKey{Outcome: "error", State: string(StateJob)}

	tests := []struct {
		name              string
		rec               *cycle.Record
		wantInjectFailed  bool
		wantDebugHeldOK   bool
		wantHeldListening bool
	}{
		{
			name:            "job OK + DEBUG record resets via debugHeldAfterJobOK",
			rec:             &cycle.Record{States: []cycle.StateRecord{jobRec(cycle.OutcomeOK), debugRec()}},
			wantDebugHeldOK: true,
		},
		{
			name: "job OK without a DEBUG record does NOT reset via debugHeldAfterJobOK",
			rec:  &cycle.Record{States: []cycle.StateRecord{jobRec(cycle.OutcomeOK)}},
		},
		{
			name: "failed DEBUG entry (no DEBUG StateRecord) stays out of the reset arm",
			rec:  &cycle.Record{States: []cycle.StateRecord{jobRec(cycle.OutcomeOK)}},
			// no DEBUG record → debugHeldAfterJobOK false even though job OK.
		},
		{
			name:              "LISTENING install failure counts WITH a ≥10-min record (heldListening carve-out)",
			rec:               &cycle.Record{States: []cycle.StateRecord{longListeningRec()}, InjectedKeys: []cycle.InjectedKey{listeningErr}},
			wantInjectFailed:  true, // injectionFailed overrides the heldListening reset
			wantHeldListening: true,
		},
		{
			name: "mid-job install failure does NOT count (injectionFailed narrowed to State!=JOB)",
			rec:  &cycle.Record{States: []cycle.StateRecord{jobRec(cycle.OutcomeError)}, InjectedKeys: []cycle.InjectedKey{jobErr}},
			// wantInjectFailed stays false: a JOB-state error is not a health signal.
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := injectionFailed(tt.rec); got != tt.wantInjectFailed {
				t.Errorf("injectionFailed = %v, want %v", got, tt.wantInjectFailed)
			}
			if got := debugHeldAfterJobOK(tt.rec); got != tt.wantDebugHeldOK {
				t.Errorf("debugHeldAfterJobOK = %v, want %v", got, tt.wantDebugHeldOK)
			}
			if got := heldListening(tt.rec); got != tt.wantHeldListening {
				t.Errorf("heldListening = %v, want %v", got, tt.wantHeldListening)
			}
		})
	}
}

func TestMidJobInstallFailureDoesNotCountStreak(t *testing.T) {
	// Decision 18: a mid-job install failure followed by a job that FAILS counts
	// the job's failure but NOT the injection — injectionFailed is narrowed to
	// State != JOB, so it keeps the streak from double-counting the operator's
	// errored attempt. Here the job then SUCCEEDS, so the streak resets entirely.
	h := newHarness(t, func(c *home.Config) {
		c.Limits.MaxJobDuration = home.Duration(10 * time.Second)
	})
	h.images.maxCalls = 1
	h.guest.installErr = errors.New("ambiguous failure")
	h.slot.mu.Lock()
	h.slot.failures = 2
	h.slot.mu.Unlock()
	cancel := h.start(t)
	defer cancel()
	h.waitState(t, StateProvision)
	h.proc.say("Listening for Jobs")
	h.waitState(t, StateListening)
	h.proc.say("Running job: build")
	h.waitState(t, StateJob)

	if r := h.debugCmd(t, nil); r.Err == nil {
		t.Fatal("want a mid-job install failure")
	}
	if h.slot.Status().DebugHoldArmed {
		t.Error("a failed install must not arm")
	}
	// The job then succeeds: the cycle is a SUCCESS, streak resets to 0, and the
	// errored JOB-state injection never counted.
	h.proc.say("Job build completed with result: Succeeded")
	h.proc.exit(0)
	h.waitState(t, StateTeardown)
	bk := h.waitState(t, StateBackoff)
	if bk.ConsecutiveFailures != 0 {
		t.Errorf("a successful job after a mid-job install failure must reset; failures=%d", bk.ConsecutiveFailures)
	}
	rec := h.jobRecord(t)
	if injectionFailed(rec) {
		t.Error("a JOB-state install error must NOT register as injectionFailed (decision 18)")
	}
	// The contamination is still on the record.
	if rec.Job == nil || len(rec.Job.OperatorKeys) != 1 {
		t.Errorf("attempted mid-job contamination missing from Job.OperatorKeys: %+v", rec.Job)
	}
}

func TestJobNameFromMarker(t *testing.T) {
	for _, tt := range []struct{ name, line, want string }{
		{"plain", "Running job: build (1)", "build (1)"},
		{"trims surrounding space", "2026-01-01 Running job:   deploy  ", "deploy"},
		{"empty after marker", "Running job:", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := jobNameFromMarker(tt.line); got != tt.want {
				t.Errorf("jobNameFromMarker(%q) = %q, want %q", tt.line, got, tt.want)
			}
		})
	}

	// A guest controls the marker line; without a cap a 1 MB name lands in
	// cycle.json and live Status.Job. The cap must hold AND keep valid UTF-8 —
	// a naive byte slice would split a multibyte rune into a U+FFFD.
	huge := "Running job: " + strings.Repeat("世", 500_000) // 3 bytes/rune, well over the cap
	got := jobNameFromMarker(huge)
	if len(got) > maxJobName {
		t.Errorf("job name not capped: %d bytes (want <= %d)", len(got), maxJobName)
	}
	if !utf8.ValidString(got) {
		t.Errorf("capped job name is not valid UTF-8: %q", got)
	}
}

// ---- observability events (ADR-0024, issue #224) ---------------------------

// TestObsEventsCleanSuccessCycle pins the event shape of a cycle that runs a
// job to completion: framed by CycleStarted/CycleFinished, one
// StepEntered/StepLeft pair per StateRecord matching cycle.json exactly,
// VMInfo published at BOOT (MAC) and AWAIT_IP (IP), a Detail from the image
// ensurer's report callback, and JobStarted/JobEnded bracketing the job.
func TestObsEventsCleanSuccessCycle(t *testing.T) {
	h := newHarness(t, nil)
	cancel := h.start(t)

	h.waitState(t, StateProvision)
	h.proc.say("Listening for Jobs")
	h.waitState(t, StateListening)
	h.proc.say("Running job: build (mac, self-hosted)")
	h.waitState(t, StateJob)
	h.proc.say("Job build completed with result: Succeeded")
	h.proc.exit(0)
	h.waitState(t, StateTeardown)
	h.waitState(t, StateBackoff)
	cancel()
	<-h.runDone

	recs := h.records(t)
	var rec *cycle.Record
	for _, r := range recs {
		if r.Result == cycle.ResultSuccess {
			rec = r
		}
	}
	if rec == nil {
		t.Fatalf("no success record in %d records", len(recs))
	}

	events := h.eventsForCycle(rec.CycleID)
	assertCycleFramed(t, events, rec)
	assertStepEventsMatchRecord(t, events, rec)

	var sawMAC, sawIP, sawDetail, sawJobStarted, sawJobEnded bool
	for _, e := range events {
		switch e.Kind {
		case obs.KindVMInfo:
			if e.VM.MAC != "" {
				sawMAC = true
			}
			if e.VM.IP != "" {
				sawIP = true
			}
		case obs.KindDetail:
			sawDetail = true
		case obs.KindJobStarted:
			sawJobStarted = true
			if e.Job.Name != "build (mac, self-hosted)" {
				t.Errorf("JobStarted name = %q", e.Job.Name)
			}
			if e.Step != string(StateJob) {
				t.Errorf("JobStarted step = %q, want JOB", e.Step)
			}
		case obs.KindJobEnded:
			sawJobEnded = true
			if e.Job.Outcome != obs.OutcomeOK {
				t.Errorf("JobEnded outcome = %q, want ok", e.Job.Outcome)
			}
		}
	}
	if !sawMAC {
		t.Error("no VMInfo event carried a MAC")
	}
	if !sawIP {
		t.Error("no VMInfo event carried an IP")
	}
	if !sawDetail {
		t.Error("no Detail event from the image ensurer's report callback")
	}
	if !sawJobStarted || !sawJobEnded {
		t.Errorf("JobStarted=%v JobEnded=%v, want both", sawJobStarted, sawJobEnded)
	}
}

// TestObsEventsErrorOutcome pins a plain (non-deadline) failure's StepLeft:
// the runner exiting before reaching LISTENING fails PROVISION with
// cycle.OutcomeError, and the obs stream must classify it the same way.
func TestObsEventsErrorOutcome(t *testing.T) {
	h := newHarness(t, nil)
	cancel := h.start(t)
	defer cancel()

	h.waitState(t, StateProvision)
	h.proc.exit(2) // runner dies before listening
	h.waitState(t, StateBackoff)

	recs := h.records(t)
	rec := recs[0]
	if rec.Failure == nil || rec.Failure.State != string(StateProvision) {
		t.Fatalf("failure = %+v, want PROVISION", rec.Failure)
	}

	events := h.eventsForCycle(rec.CycleID)
	assertCycleFramed(t, events, rec)
	assertStepEventsMatchRecord(t, events, rec)
}

// TestObsEventsDeadlineOutcome pins a deadline failure's StepLeft: AWAIT_IP
// timing out classifies as cycle.OutcomeDeadline in cycle.json, and the obs
// stream carries the same outcome string (obs.Outcome is a plain string, not
// restricted to ok/error — see internal/obs).
func TestObsEventsDeadlineOutcome(t *testing.T) {
	h := newHarness(t, nil)
	h.vmF.machine.ip = "" // WaitIP never resolves -> AWAIT_IP deadline
	cancel := h.start(t)
	defer cancel()

	h.waitState(t, StateAwaitIP)
	h.waitState(t, StateBackoff)

	recs := h.records(t)
	rec := recs[0]
	if rec.Failure == nil || rec.Failure.State != string(StateAwaitIP) {
		t.Fatalf("failure = %+v, want AWAIT_IP", rec.Failure)
	}

	events := h.eventsForCycle(rec.CycleID)
	assertCycleFramed(t, events, rec)
	assertStepEventsMatchRecord(t, events, rec)
}

// TestObsEventsOperatorRecycle pins the benign-recycle shape: Ending
// "recycle" on both the record and CycleFinished, with the LISTENING step
// left "ok" (the recycle interrupts between states, not mid-state).
func TestObsEventsOperatorRecycle(t *testing.T) {
	h := newHarness(t, nil)
	cancel := h.start(t)
	defer cancel()

	h.waitState(t, StateProvision)
	h.proc.say(markerListening)
	h.waitState(t, StateListening)

	if !h.slot.Command(Command{Kind: CmdRecycle, Reason: "image bump"}) {
		t.Fatal("command rejected")
	}
	h.waitState(t, StateTeardown)
	h.waitState(t, StateBackoff)

	recs := h.records(t)
	rec := recs[0]
	if rec.Ending != cycle.EndingRecycle {
		t.Fatalf("Ending = %q, want %q", rec.Ending, cycle.EndingRecycle)
	}

	events := h.eventsForCycle(rec.CycleID)
	assertCycleFramed(t, events, rec)
	assertStepEventsMatchRecord(t, events, rec)
}

// TestObsEventsDebugHold pins a debug-hold cycle's shape: the DEBUG
// StepEntered/StepLeft pair, plus the audit trail's AuditAppend (write-ahead
// "pending") and AuditUpdate ("ok") events carrying the operator's
// fingerprint — the audit events are observational copies of
// rec.InjectedKeys, never the system of record.
func TestObsEventsDebugHold(t *testing.T) {
	h := newHarness(t, nil)
	h.images.maxCalls = 1
	cancel := h.reachListening(t)
	defer cancel()

	r := h.debugCmd(t, nil)
	if r.Err != nil {
		t.Fatalf("freeze failed: %v", r.Err)
	}
	h.waitState(t, StateDebug)
	h.slot.Command(Command{Kind: CmdRecycle, Reason: "done debugging"})
	h.waitState(t, StateTeardown)
	h.waitState(t, StateBackoff)

	recs := h.records(t)
	rec := recs[0]
	var sawDebug bool
	for _, sr := range rec.States {
		if sr.State == string(StateDebug) {
			sawDebug = true
		}
	}
	if !sawDebug {
		t.Fatalf("no DEBUG StateRecord: %+v", rec.States)
	}

	events := h.eventsForCycle(rec.CycleID)
	assertCycleFramed(t, events, rec)
	assertStepEventsMatchRecord(t, events, rec)

	var sawPending, sawOK bool
	for _, e := range events {
		if e.Kind != obs.KindAuditAppend && e.Kind != obs.KindAuditUpdate {
			continue
		}
		if e.Audit.Fingerprint != "SHA256:testfp" {
			t.Errorf("audit event fingerprint = %q, want SHA256:testfp", e.Audit.Fingerprint)
		}
		switch e.Audit.Outcome {
		case "pending":
			sawPending = true
		case "ok":
			sawOK = true
		}
	}
	if !sawPending || !sawOK {
		t.Errorf("sawPending=%v sawOK=%v, want both", sawPending, sawOK)
	}
}

// TestObsEventsWedge pins a wedge's shape: TEARDOWN's StepLeft outcome
// "error" (force-stop failed, guest still running) with Ending "wedge" on
// both the record and CycleFinished.
func TestObsEventsWedge(t *testing.T) {
	h := newHarness(t, nil)
	h.vmF.machine.stopErr = errors.New("force stop failed with guest still running")
	h.vmF.machine.ip = "" // fail at AWAIT_IP so teardown owns a booted machine
	cancel := h.start(t)
	_ = cancel

	h.waitState(t, StateTeardown)
	select {
	case <-h.runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("slot did not park after the guest survived force-stop")
	}

	recs := h.records(t)
	rec := recs[0]
	if rec.Ending != cycle.EndingWedge {
		t.Fatalf("Ending = %q, want %q", rec.Ending, cycle.EndingWedge)
	}

	events := h.eventsForCycle(rec.CycleID)
	assertCycleFramed(t, events, rec)
	assertStepEventsMatchRecord(t, events, rec)
}

// TestObsEventsNilHookIsNoop pins that a Deps with no Events hook (the
// zero-value default every other test in this file already exercises)
// drives a cycle exactly as before: obs.WithCycle/Emit/Action degrade to
// no-ops on a nil emitter, so nothing here should ever observe an event.
func TestObsEventsNilHookIsNoop(t *testing.T) {
	h := newHarness(t, nil)
	h.slot.deps.Events = nil // the harness wires one by default; unwire it

	cancel := h.start(t)
	h.waitState(t, StateProvision)
	h.proc.say("Listening for Jobs")
	h.waitState(t, StateListening)
	h.proc.say("Running job: build")
	h.waitState(t, StateJob)
	h.proc.say("Job build completed with result: Succeeded")
	h.proc.exit(0)
	h.waitState(t, StateTeardown)
	h.waitState(t, StateBackoff)
	cancel()
	<-h.runDone

	h.eventsMu.Lock()
	n := len(h.events)
	h.eventsMu.Unlock()
	if n != 0 {
		t.Errorf("got %d events with no Events hook installed, want 0", n)
	}
}
