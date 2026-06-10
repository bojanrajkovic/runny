package statemachine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bojanrajkovic/runny/internal/cycle"
	"github.com/bojanrajkovic/runny/internal/github"
	"github.com/bojanrajkovic/runny/internal/home"
	"github.com/bojanrajkovic/runny/internal/tart"
	"github.com/bojanrajkovic/runny/internal/vm"
)

// ---- fakes -----------------------------------------------------------------

type fakeImages struct {
	bundle tart.Bundle
	err    error
	// maxCalls > 0: calls beyond it block until ctx ends, so tests control
	// exactly how many cycles run.
	maxCalls int
	calls    int
	mu       sync.Mutex
}

func (f *fakeImages) Ensure(ctx context.Context, report func(string)) (string, tart.Bundle, error) {
	if report != nil {
		report("pulled 1.0 MiB at 1.0 MiB/s")
	}
	f.mu.Lock()
	f.calls++
	blocked := f.maxCalls > 0 && f.calls > f.maxCalls
	f.mu.Unlock()
	if blocked {
		<-ctx.Done()
		return "", "", ctx.Err()
	}
	return "sha256:fake", f.bundle, f.err
}

type fakeMachine struct {
	mac     string
	ip      string
	ipErr   error
	done    chan struct{}
	stopped bool
	mu      sync.Mutex
}

func (m *fakeMachine) MAC() string { return m.mac }
func (m *fakeMachine) WaitIP(ctx context.Context) (string, error) {
	if m.ipErr != nil {
		return "", m.ipErr
	}
	if m.ip == "" { // simulate never-arriving lease
		<-ctx.Done()
		return "", ctx.Err()
	}
	return m.ip, nil
}

func (m *fakeMachine) Stop(context.Context, time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.stopped {
		m.stopped = true
		close(m.done)
	}
	return nil
}
func (m *fakeMachine) Done() <-chan struct{} { return m.done }

type fakeVM struct {
	machine *fakeMachine
	bootErr error
	boots   int
	mu      sync.Mutex
}

func (f *fakeVM) Boot(ctx context.Context, b tart.Bundle, o vm.BootOptions) (vm.Machine, error) {
	f.mu.Lock()
	f.boots++
	f.mu.Unlock()
	if f.bootErr != nil {
		return nil, f.bootErr
	}
	return f.machine, nil
}

type fakeProc struct {
	lines chan string
	code  int
	done  chan struct{}
	once  sync.Once
}

func newFakeProc() *fakeProc {
	return &fakeProc{lines: make(chan string, 16), done: make(chan struct{})}
}

func (p *fakeProc) Lines() <-chan string { return p.lines }
func (p *fakeProc) Wait() (int, error)   { <-p.done; return p.code, nil }
func (p *fakeProc) Kill()                { p.exit(p.code) }
func (p *fakeProc) say(s string)         { p.lines <- s }
func (p *fakeProc) exit(code int) {
	p.once.Do(func() {
		p.code = code
		close(p.lines)
		close(p.done)
	})
}

type fakeGuest struct {
	proc     *fakeProc
	startErr error
	diag     []byte
	diagErr  error
	pulled   bool
	mu       sync.Mutex
}

func (g *fakeGuest) StartRunner(ctx context.Context, jit string) (Proc, error) {
	if g.startErr != nil {
		return nil, g.startErr
	}
	return g.proc, nil
}

func (g *fakeGuest) PullDiag(context.Context) ([]byte, error) {
	g.mu.Lock()
	g.pulled = true
	g.mu.Unlock()
	return g.diag, g.diagErr
}
func (g *fakeGuest) Close() error { return nil }

type fakeDialer struct {
	guest *fakeGuest
	err   error
}

func (d *fakeDialer) WaitFor(ctx context.Context, addr string) (Guest, error) {
	if d.err != nil {
		return nil, d.err
	}
	return d.guest, nil
}

type fakeGitHub struct {
	mu         sync.Mutex
	registered map[string]bool
	deleted    []int64
	nextID     int64
	listErr    error
	dropAll    bool
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{registered: map[string]bool{}, nextID: 100}
}

func (g *fakeGitHub) GenerateJITConfig(ctx context.Context, name string, labels []string, group int64) (*github.JITRunner, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.nextID++
	g.registered[name] = true
	return &github.JITRunner{RunnerID: g.nextID, EncodedJITConfig: "aml0"}, nil
}

func (g *fakeGitHub) ListRunners(ctx context.Context) ([]github.Runner, error) {
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

func (g *fakeGitHub) DeleteRunner(ctx context.Context, id int64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.deleted = append(g.deleted, id)
	return nil
}

// ---- harness ----------------------------------------------------------------

type harness struct {
	slot    *Slot
	vmF     *fakeVM
	proc    *fakeProc
	guest   *fakeGuest
	gh      *fakeGitHub
	images  *fakeImages
	dir     home.Dir
	states  chan Status
	runDone chan struct{}
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
	t.Helper()
	dir := home.Dir(t.TempDir())
	if err := dir.Ensure(); err != nil {
		t.Fatal(err)
	}
	cfg := &home.Config{}
	// Defaults via the real path: write a minimal config through LoadConfig
	// would need files; call the exported-for-test defaulting instead.
	cfg.GitHub.Labels = []string{"self-hosted"}
	cfg.GitHub.RunnerGroupID = 1
	cfg.Runners.NamePrefix = "runny"
	set := func(d *home.Duration, v time.Duration) { *d = home.Duration(v) }
	set(&cfg.Deadlines.Clone, time.Second)
	set(&cfg.Deadlines.Boot, time.Second)
	set(&cfg.Deadlines.AwaitIP, 500*time.Millisecond)
	set(&cfg.Deadlines.AwaitSSH, time.Second)
	set(&cfg.Deadlines.MintJIT, time.Second)
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
	deps := Deps{
		Home:   dir,
		Config: cfg,
		VM:     h.vmF,
		Images: h.images,
		Clone: func(src tart.Bundle, dst string) error {
			return os.MkdirAll(dst, 0o755)
		},
		GitHub: h.gh,
		Dial:   &fakeDialer{guest: h.guest},
	}
	h.slot = NewSlot("runner-1", deps)
	h.slot.OnChange(func(st Status) {
		select {
		case h.states <- st:
		default:
		}
	})
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
	recs, err := cycle.Store{SlotDir: h.dir.SlotCyclesDir("runner-1")}.Recent(0)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

// ---- tests -------------------------------------------------------------------

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

	h.proc.say("Job build completed with result: Succeeded")
	h.proc.exit(0)
	h.waitState(t, StateTeardown)
	h.waitState(t, StateBackoff)
	cancel()
	// record written before BACKOFF; single gated cycle

	recs := h.records(t)
	if len(recs) != 1 {
		t.Fatalf("got %d records", len(recs))
	}
	rec := recs[0]
	if rec.Result != cycle.ResultSuccess {
		t.Errorf("result = %s; failure = %+v", rec.Result, rec.Failure)
	}
	if rec.Job == nil {
		t.Error("job not recorded")
	}
	// Job ran → JIT runner self-removes → no explicit deletion.
	if len(h.gh.deleted) != 0 {
		t.Errorf("deleted %v, want none after a completed job", h.gh.deleted)
	}
	if h.slot.Status().ConsecutiveFailures != 0 {
		t.Error("failures should reset after success")
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
	h.waitState(t, StateBackoff)
	cancel()

	recs := h.records(t)
	if !strings.Contains(recs[0].Failure.Error, "recycled by operator") {
		t.Errorf("failure = %+v", recs[0].Failure)
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

var _ = fmt.Sprintf // keep fmt for debug edits
