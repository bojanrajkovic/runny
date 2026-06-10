// Package statemachine is runny's core: one crash-only FSM per runner slot
// (ADR-0004). Every state is entered with a context deadline; the only
// response to any failure is TEARDOWN → BACKOFF with capped exponential
// backoff. Teardown cannot fail — escalating force is the floor.
package statemachine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bojanrajkovic/runny/internal/bounded"
	"github.com/bojanrajkovic/runny/internal/cycle"
	"github.com/bojanrajkovic/runny/internal/github"
	"github.com/bojanrajkovic/runny/internal/home"
	"github.com/bojanrajkovic/runny/internal/tart"
	"github.com/bojanrajkovic/runny/internal/vm"
)

// State names; the proto enum mirrors these.
type State string

const (
	StateBackoff     State = "BACKOFF"
	StateEnsureImage State = "ENSURE_IMAGE"
	StateClone       State = "CLONE"
	StateBoot        State = "BOOT"
	StateAwaitIP     State = "AWAIT_IP"
	StateAwaitSSH    State = "AWAIT_SSH"
	StateMintJIT     State = "MINT_JIT"
	StateProvision   State = "PROVISION"
	StateListening   State = "LISTENING"
	StateJob         State = "JOB"
	StateTeardown    State = "TEARDOWN"
)

// Runner-output markers (the actions runner's run.sh wording).
const (
	markerListening    = "Listening for Jobs"
	markerJobStarted   = "Running job:"
	markerJobCompleted = "completed with result:"
)

// ImageEnsurer makes sure the configured image is cached locally and returns
// its digest and bundle dir (ENSURE_IMAGE's work). report receives live
// progress annotations ("2.1 GiB at 41 MiB/s") — pull progress must be
// visible, not just stall-detected, so an operator can tell slow-registry
// from stuck (the predecessor made them indistinguishable).
type ImageEnsurer interface {
	Ensure(ctx context.Context, report func(detail string)) (digest string, bundle tart.Bundle, err error)
}

// Cloner clones a bundle (tart.Clone's seam).
type Cloner func(src tart.Bundle, dst string) error

// GitHub is the slice of internal/github the FSM needs. Every method takes
// bounded.Context: these are network calls, and an unbounded call site is a
// compile error (ADR-0011).
type GitHub interface {
	GenerateJITConfig(ctx bounded.Context, name string, labels []string, groupID int64) (*github.JITRunner, error)
	ListRunners(ctx bounded.Context) ([]github.Runner, error)
	DeleteRunner(ctx bounded.Context, id int64) error
}

// Proc is a running guest process (the runner's run.sh).
type Proc interface {
	Lines() <-chan string
	Wait() (int, error)
	Kill()
}

// Guest is an authenticated session into a booted VM.
type Guest interface {
	// StartRunner stages the actions runner from the cache share and launches
	// run.sh with the JIT config; goos selects the per-OS provision path.
	// It deliberately takes a plain context: the ctx is the proc's LIFETIME
	// (run.sh must outlive PROVISION's deadline), not an operation bound —
	// session establishment is bounded internally by sshx's socket deadlines.
	StartRunner(ctx context.Context, jit, goos string) (Proc, error)
	// PullDiag fetches the tail of the runner's _diag logs (post-mortem).
	PullDiag(ctx bounded.Context) ([]byte, error)
	Close() error
}

// Dialer establishes Guest sessions (sshx's seam).
type Dialer interface {
	WaitFor(ctx bounded.Context, addr string) (Guest, error)
}

// Deps wires a slot to the world. Everything is an interface or func so the
// FSM tests on any OS with fakes. Images/GitHub/Dial are pool-scoped
// (per-pool image, registration target, and guest credentials — ADR-0009);
// Config carries the global deadlines/limits/retention.
type Deps struct {
	Home   home.Dir
	Config *home.Config
	Pool   home.PoolConfig
	VM     vm.Manager
	Images ImageEnsurer
	Clone  Cloner
	GitHub GitHub
	Dial   Dialer
	Log    *slog.Logger
}

// Command is an operator injection (from runnyctl via the socket).
type Command struct {
	Kind   CommandKind
	Reason string
}

type CommandKind int

const (
	CmdRecycle CommandKind = iota
	CmdPause
	CmdResume
)

// Status is the live snapshot the control surface renders.
type Status struct {
	Slot                string
	State               State
	StateEntered        time.Time
	CycleID             string
	Paused              bool
	ConsecutiveFailures uint32
	BackoffSeconds      int64
	VM                  cycle.VMInfo
	Job                 *cycle.JobInfo
	LastFailure         string
	// Detail is the current state's live annotation (pull progress etc).
	Detail string
	// Wedged: the guest survived force-stop and still occupies a
	// Virtualization.framework guest slot. The slot is parked; only a daemon
	// restart (cold start) reclaims the guest (ADR-0012).
	Wedged bool
}

// Slot drives one runner slot's lifecycle.
type Slot struct {
	name string
	deps Deps

	cmds chan Command

	mu       sync.Mutex
	status   Status
	onChange []func(Status)

	// failure streak for backoff; reset per ADR-0004.
	failures uint32
	paused   bool
}

func NewSlot(name string, deps Deps) *Slot {
	if deps.Log == nil {
		deps.Log = slog.Default()
	}
	deps.Log = deps.Log.With("slot", name)
	return &Slot{
		name: name,
		deps: deps,
		cmds: make(chan Command, 8),
	}
}

// Command injects an operator command; non-blocking (drops when the buffer
// is full rather than wedging the control surface).
func (s *Slot) Command(c Command) bool {
	select {
	case s.cmds <- c:
		return true
	default:
		return false
	}
}

// OnChange registers a status listener (the socket server's watch feed, the
// daemon's wedge watcher). Listeners are called in registration order, off
// the slot's lock.
func (s *Slot) OnChange(fn func(Status)) {
	s.mu.Lock()
	s.onChange = append(s.onChange, fn)
	s.mu.Unlock()
}

// notify calls every listener with snap (callers must not hold s.mu).
func (s *Slot) notify(fns []func(Status), snap Status) {
	for _, fn := range fns {
		fn(snap)
	}
}

// Status returns the current snapshot.
func (s *Slot) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

func (s *Slot) setState(state State, mut func(*Status)) {
	s.mu.Lock()
	s.status.Slot = s.name
	s.status.State = state
	s.status.StateEntered = time.Now()
	s.status.Detail = ""
	if mut != nil {
		mut(&s.status)
	}
	snap := s.status
	fns := slices.Clone(s.onChange)
	s.mu.Unlock()
	s.deps.Log.Info("state", "state", state, "cycle", snap.CycleID)
	s.notify(fns, snap)
}

// Run drives cycles until ctx is cancelled or the slot wedges. This is the
// slot goroutine.
func (s *Slot) Run(ctx context.Context) {
	for {
		if err := s.backoffWait(ctx); err != nil {
			return // daemon shutdown
		}
		rec, wedged := s.runCycle(ctx)
		s.finishCycle(ctx, rec)
		if wedged {
			// The guest survived force-stop: it still occupies one of the
			// host's Virtualization.framework guest slots, so every further
			// boot on this slot is doomed. Park; only a daemon restart (cold
			// start) reclaims an in-process VM (ADR-0012).
			s.markWedged()
			return
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// markWedged parks the slot and tells the world why.
func (s *Slot) markWedged() {
	s.mu.Lock()
	s.status.Wedged = true
	s.status.Detail = "guest survived force-stop; slot parked until the daemon restarts"
	snap := s.status
	fns := slices.Clone(s.onChange)
	s.mu.Unlock()
	s.deps.Log.Error("slot wedged: guest survived force-stop and holds a guest-cap slot; parking (the daemon restarts cold once no job is running)")
	s.notify(fns, snap)
}

// backoffWait sits in BACKOFF until the backoff timer elapses and the slot
// is unpaused. Commands are serviced here too (resume, mainly).
func (s *Slot) backoffWait(ctx context.Context) error {
	wait := s.currentBackoff()
	s.setState(StateBackoff, func(st *Status) {
		st.BackoffSeconds = int64(wait / time.Second)
		st.CycleID = ""
		st.VM = cycle.VMInfo{}
		st.Job = nil
	})
	timer := time.NewTimer(wait)
	defer timer.Stop()
	ready := false
	for {
		if ready && !s.isPaused() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			ready = true
		case cmd := <-s.cmds:
			s.handleIdleCommand(cmd)
		}
	}
}

func (s *Slot) handleIdleCommand(cmd Command) {
	switch cmd.Kind {
	case CmdPause:
		s.setPaused(true)
	case CmdResume:
		s.setPaused(false)
	case CmdRecycle:
		// Nothing to recycle while idle.
	}
}

// setDetail publishes a live annotation for the current state.
func (s *Slot) setDetail(detail string) {
	s.mu.Lock()
	if s.status.Detail == detail {
		s.mu.Unlock()
		return
	}
	s.status.Detail = detail
	snap := s.status
	fns := slices.Clone(s.onChange)
	s.mu.Unlock()
	s.notify(fns, snap)
}

func (s *Slot) isPaused() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.paused
}

func (s *Slot) setPaused(p bool) {
	s.mu.Lock()
	s.paused = p
	s.status.Paused = p
	snap := s.status
	fns := slices.Clone(s.onChange)
	s.mu.Unlock()
	s.notify(fns, snap)
}

func (s *Slot) currentBackoff() time.Duration {
	s.mu.Lock()
	n := s.failures
	s.mu.Unlock()
	if n == 0 {
		return 0
	}
	base := s.deps.Config.Limits.BackoffBase.D()
	capD := s.deps.Config.Limits.BackoffCap.D()
	d := base << min(n-1, 16)
	if d > capD || d <= 0 {
		return capD
	}
	return d
}

// cycleErr carries the failing state with the error into teardown.
type cycleErr struct {
	state State
	err   error
}

func (e *cycleErr) Error() string { return fmt.Sprintf("%s: %v", e.state, e.err) }

// runCycle executes states 1..9, always handing off to TEARDOWN, and returns
// the cycle record (teardown fills the tail) plus whether teardown wedged
// (force-stop failed with the guest still running).
func (s *Slot) runCycle(ctx context.Context) (*cycle.Record, bool) {
	cfg := s.deps.Config
	rec := &cycle.Record{
		CycleID: cycle.NewID(),
		Slot:    s.name,
		Started: time.Now(),
	}
	runnerName := fmt.Sprintf("%s-%s-%s", cfg.NamePrefix, s.name, rec.CycleID)

	var (
		machine   vm.Machine
		guest     Guest
		proc      Proc
		runnerID  int64
		jobRan    bool
		failState State
		failErr   error
	)

	// runState records one state's execution; sctx is consulted only to
	// classify deadline outcomes. false = cycle failed.
	runState := func(state State, sctx context.Context, f func() error) bool {
		s.setState(state, func(st *Status) { st.CycleID = rec.CycleID })
		sr := cycle.StateRecord{State: string(state), Entered: time.Now()}
		err := f()
		sr.Left = time.Now()
		switch {
		case err == nil:
			sr.Outcome = cycle.OutcomeOK
		case errors.Is(err, context.DeadlineExceeded) || sctx.Err() != nil && errors.Is(context.Cause(sctx), context.DeadlineExceeded):
			sr.Outcome, sr.Error = cycle.OutcomeDeadline, err.Error()
		default:
			sr.Outcome, sr.Error = cycle.OutcomeError, err.Error()
		}
		rec.States = append(rec.States, sr)
		if err != nil {
			failState, failErr = state, err
			return false
		}
		return true
	}

	// enter runs one deadline-bounded state. The state function receives the
	// deadline as a bounded.Context it can hand straight to the guest and
	// network seams — the per-state deadline is the contract, and the type
	// system carries it to the call sites (ADR-0011).
	enter := func(state State, d time.Duration, f func(bounded.Context) error) bool {
		bctx, cancel := bounded.WithTimeout(ctx, d)
		ok := runState(state, bctx, func() error { return f(bctx) })
		cancel()
		return ok
	}

	var srcBundle tart.Bundle
	// ENSURE_IMAGE is the one state with no wall-clock deadline — pull
	// duration is unknowable — so it runs under the cycle context; its
	// operations carry their own bounds (resolve timeout, stall watcher).
	ok := runState(StateEnsureImage, ctx, func() error {
		digest, bundle, err := s.deps.Images.Ensure(ctx, s.setDetail)
		if err != nil {
			return err
		}
		rec.ImageDigest = digest
		srcBundle = bundle
		return nil
	})

	vmDir := s.deps.Home.VMDir(s.name)
	if ok {
		ok = enter(StateClone, cfg.Deadlines.Clone.D(), func(c bounded.Context) error {
			return s.deps.Clone(srcBundle, vmDir)
		})
	}
	if ok {
		ok = enter(StateBoot, cfg.Deadlines.Boot.D(), func(c bounded.Context) error {
			m, err := s.deps.VM.Boot(c, tart.Bundle(vmDir), vm.BootOptions{
				RunnerCacheDir: s.deps.Home.RunnerCacheDir(),
			})
			if err != nil {
				return err
			}
			machine = m
			s.mu.Lock()
			s.status.VM.MAC = m.MAC()
			s.mu.Unlock()
			rec.VM.MAC = m.MAC()
			return nil
		})
	}
	var ip string
	if ok {
		ok = enter(StateAwaitIP, cfg.Deadlines.AwaitIP.D(), func(c bounded.Context) error {
			got, err := machine.WaitIP(c)
			if err != nil {
				return err
			}
			ip = got
			s.mu.Lock()
			s.status.VM.IP = ip
			s.mu.Unlock()
			rec.VM.IP = ip
			return nil
		})
	}
	if ok {
		ok = enter(StateAwaitSSH, cfg.Deadlines.AwaitSSH.D(), func(c bounded.Context) error {
			g, err := s.deps.Dial.WaitFor(c, ip+":22")
			if err != nil {
				return err
			}
			guest = g
			return nil
		})
	}
	var jit *github.JITRunner
	if ok {
		ok = enter(StateMintJIT, cfg.Deadlines.MintJIT.D(), func(c bounded.Context) error {
			j, err := s.deps.GitHub.GenerateJITConfig(c, runnerName, s.deps.Pool.Labels, s.deps.Pool.RunnerGroupID)
			if err != nil {
				return err
			}
			jit = j
			runnerID = j.RunnerID
			return nil
		})
	}
	if ok {
		ok = enter(StateProvision, cfg.Deadlines.Provision.D(), func(c bounded.Context) error {
			// The proc must outlive this state's deadline: start it under the
			// cycle ctx, but bound the wait-for-listening here.
			p, err := guest.StartRunner(ctx, jit.EncodedJITConfig, s.deps.Pool.OS)
			if err != nil {
				return err
			}
			proc = p
			for {
				select {
				case <-c.Done():
					return fmt.Errorf("runner did not reach %q: %w", markerListening, context.Cause(c))
				case line, open := <-p.Lines():
					if !open {
						code, _ := p.Wait()
						return fmt.Errorf("runner exited (code %d) before listening", code)
					}
					if strings.Contains(line, markerListening) {
						return nil
					}
				}
			}
		})
	}

	// LISTENING / JOB: watch-driven, no fixed deadline; budgets come from the
	// watches themselves.
	if ok {
		jobRan, failState, failErr = s.listenAndRunJob(ctx, rec, proc, runnerName)
		ok = failState == ""
	}

	// TEARDOWN — unconditional. Force is the floor; a guest that survives
	// even force-stop wedges the slot, and the record says so truthfully.
	wedged := s.teardown(ctx, rec, teardownInputs{
		machine:  machine,
		guest:    guest,
		proc:     proc,
		runnerID: runnerID,
		jobRan:   jobRan,
		vmDir:    vmDir,
		failed:   !ok,
	})

	rec.Finished = time.Now()
	switch {
	case ok && !wedged:
		rec.Result = cycle.ResultSuccess
	case !ok:
		rec.Result = cycle.ResultFailure
		rec.Failure = &cycle.Failure{State: string(failState), Error: failErr.Error()}
	default: // the cycle succeeded but its teardown could not kill the guest
		rec.Result = cycle.ResultFailure
		rec.Failure = &cycle.Failure{State: string(StateTeardown), Error: "vm stop escalation failed; guest still running"}
	}
	return rec, wedged
}

// listenAndRunJob handles LISTENING and JOB. Returns jobRan and, on failure,
// the failing state + error (empty state = clean completion).
func (s *Slot) listenAndRunJob(ctx context.Context, rec *cycle.Record, proc Proc, runnerName string) (bool, State, error) {
	cfg := s.deps.Config
	s.setState(StateListening, nil)
	lrec := cycle.StateRecord{State: string(StateListening), Entered: time.Now()}

	reconcile := time.NewTicker(cfg.Limits.ReconcileInterval.D())
	defer reconcile.Stop()
	maxIdle := time.NewTimer(cfg.Limits.MaxIdle.D())
	defer maxIdle.Stop()

	finishListening := func(outcome cycle.Outcome, err string) {
		lrec.Left, lrec.Outcome, lrec.Error = time.Now(), outcome, err
		rec.States = append(rec.States, lrec)
	}

	for {
		select {
		case <-ctx.Done():
			finishListening(cycle.OutcomeError, "daemon shutdown")
			return false, StateListening, ctx.Err()

		case cmd := <-s.cmds:
			switch cmd.Kind {
			case CmdRecycle:
				finishListening(cycle.OutcomeOK, "")
				return false, StateListening, fmt.Errorf("recycled by operator: %s", cmd.Reason)
			case CmdPause:
				s.setPaused(true) // takes effect at next BACKOFF
			case CmdResume:
				s.setPaused(false)
			}

		case <-maxIdle.C:
			// Not a failure: recycle to absorb image updates.
			finishListening(cycle.OutcomeOK, "")
			return false, StateListening, fmt.Errorf("max idle (%v) reached", cfg.Limits.MaxIdle.D())

		case <-reconcile.C:
			// The interval doubles as the budget: a registration check
			// slower than its own cadence is already pathological, and the
			// LISTENING select must not be blockable past one tick.
			rctx, rcancel := bounded.WithTimeout(ctx, cfg.Limits.ReconcileInterval.D())
			runners, err := s.deps.GitHub.ListRunners(rctx)
			rcancel()
			if err != nil {
				// GitHub unreachable is transient: hold and log, never recycle
				// for it — sand's DNS-blip lesson.
				s.deps.Log.Warn("reconcile failed; holding", "err", err)
				continue
			}
			if !runnerRegistered(runners, runnerName) {
				finishListening(cycle.OutcomeError, "registration vanished")
				return false, StateListening, errors.New("zombie: registration vanished while listening")
			}

		case line, open := <-proc.Lines():
			if !open {
				code, _ := proc.Wait()
				finishListening(cycle.OutcomeError, fmt.Sprintf("runner exited (code %d) while idle", code))
				return false, StateListening, fmt.Errorf("runner exited (code %d) while listening", code)
			}
			if !strings.Contains(line, markerJobStarted) {
				continue
			}
			// → JOB
			finishListening(cycle.OutcomeOK, "")
			jobName := strings.TrimSpace(line[strings.Index(line, markerJobStarted)+len(markerJobStarted):])
			job := &cycle.JobInfo{Name: jobName, Started: time.Now()}
			rec.Job = job
			s.setState(StateJob, func(st *Status) { st.Job = job })
			jrec := cycle.StateRecord{State: string(StateJob), Entered: time.Now()}

			jctx, cancel := context.WithTimeout(ctx, cfg.Limits.MaxJobDuration.D())
			ok, err := s.watchJob(jctx, proc)
			cancel()
			jrec.Left = time.Now()
			if ok {
				jrec.Outcome = cycle.OutcomeOK
				rec.States = append(rec.States, jrec)
				return true, "", nil
			}
			jrec.Outcome, jrec.Error = cycle.OutcomeError, err.Error()
			rec.States = append(rec.States, jrec)
			return true, StateJob, err
		}
	}
}

func (s *Slot) watchJob(ctx context.Context, proc Proc) (bool, error) {
	for {
		select {
		case <-ctx.Done():
			return false, fmt.Errorf("job exceeded budget: %w", context.Cause(ctx))
		case line, open := <-proc.Lines():
			if !open {
				// Ephemeral runners exit right after the job; treat exit
				// during JOB as completion-by-exit (the completion line
				// usually precedes it, but output can race the exit).
				code, _ := proc.Wait()
				if code == 0 {
					return true, nil
				}
				return false, fmt.Errorf("runner exited mid-job (code %d)", code)
			}
			if strings.Contains(line, markerJobCompleted) {
				return true, nil
			}
		}
	}
}

type teardownInputs struct {
	machine  vm.Machine
	guest    Guest
	proc     Proc
	runnerID int64
	jobRan   bool
	vmDir    string
	failed   bool
}

// teardown is the universal sink. Post-mortem first (failure cycles), then
// stop → delete → deregister → record. Every step is best-effort with its own
// bound; nothing here can wedge the slot. Returns true when force-stop
// failed with the guest still running — the one failure teardown cannot
// absorb, because releasing an in-process VM takes a process exit.
func (s *Slot) teardown(ctx context.Context, rec *cycle.Record, in teardownInputs) bool {
	cfg := s.deps.Config
	s.setState(StateTeardown, nil)
	tr := cycle.StateRecord{State: string(StateTeardown), Entered: time.Now()}
	// Teardown must run even when ctx (daemon shutdown) is done: detach.
	tctx, cancel := bounded.WithTimeout(context.WithoutCancel(ctx), cfg.Deadlines.Teardown.D())
	defer cancel()

	store := cycle.Store{SlotDir: s.deps.Home.SlotCyclesDir(s.name)}

	// 1. Post-mortem while the guest still exists (failure cycles only).
	if in.failed && in.guest != nil {
		pctx, pcancel := bounded.WithTimeout(tctx, 15*time.Second)
		if diag, err := in.guest.PullDiag(pctx); err == nil && len(diag) > 0 {
			if dir, derr := store.Dir(rec); derr == nil {
				if werr := writeFile(dir, "runner-diag.log", diag); werr == nil {
					rec.Artifacts = append(rec.Artifacts, "runner-diag.log")
				}
			}
		} else if err != nil {
			s.deps.Log.Debug("post-mortem pull failed", "err", err)
		}
		pcancel()
	}

	// 2. Kill the runner proc and close the session.
	if in.proc != nil {
		in.proc.Kill()
	}
	if in.guest != nil {
		_ = in.guest.Close()
	}

	// 3. Stop the VM (graceful 10s → force; force is the floor).
	wedged := false
	if in.machine != nil {
		if err := in.machine.Stop(tctx, 10*time.Second); err != nil {
			s.deps.Log.Error("vm stop escalation failed; guest still running", "err", err)
			wedged = true
			tr.Error = fmt.Sprintf("vm stop escalation failed: %v", err)
		}
	}

	// 4. Delete the clone bundle — unless the undead guest still holds it.
	// Deleting the disk out from under a live guest destroys the evidence
	// and frees nothing that matters (the guest-cap slot stays occupied).
	if !wedged {
		if err := removeAll(in.vmDir); err != nil {
			s.deps.Log.Error("removing vm dir", "err", err)
		}
	}

	// 5. Deregister iff no job ran (JIT runners self-remove after a job).
	if in.runnerID != 0 && !in.jobRan {
		if err := s.deps.GitHub.DeleteRunner(tctx, in.runnerID); err != nil {
			s.deps.Log.Warn("deregistering runner", "id", in.runnerID, "err", err)
		}
	}

	tr.Left = time.Now()
	if wedged {
		// Recording OK here once hid the exact outage this project exists
		// to kill: cycle.json swore teardown succeeded while a ghost guest
		// ate the macOS guest cap and every later boot failed.
		tr.Outcome = cycle.OutcomeError
	} else {
		tr.Outcome = cycle.OutcomeOK
	}
	rec.States = append(rec.States, tr)
	return wedged
}

// finishCycle writes the record, updates failure accounting, and prunes.
func (s *Slot) finishCycle(ctx context.Context, rec *cycle.Record) {
	store := cycle.Store{SlotDir: s.deps.Home.SlotCyclesDir(s.name)}
	if err := store.Write(rec); err != nil {
		s.deps.Log.Error("writing cycle record", "err", err)
	}
	cfg := s.deps.Config
	_ = store.Prune(cfg.Retention.CyclesPerSlot, cfg.Retention.MaxAge.D(), time.Now())

	s.mu.Lock()
	if rec.Result == cycle.ResultSuccess || heldListening(rec) {
		s.failures = 0
		s.status.LastFailure = ""
	} else {
		s.failures++
		if rec.Failure != nil {
			s.status.LastFailure = rec.Failure.State + ": " + rec.Failure.Error
		}
	}
	s.status.ConsecutiveFailures = s.failures
	s.mu.Unlock()
	s.deps.Log.Info("cycle finished", "cycle", rec.CycleID, "result", rec.Result)
}

// heldListening: a cycle that listened ≥10min resets backoff even if it ended
// in a non-success (e.g. operator recycle, zombie after hours) — ADR-0004.
func heldListening(rec *cycle.Record) bool {
	for _, sr := range rec.States {
		if sr.State == string(StateListening) && sr.Left.Sub(sr.Entered) >= 10*time.Minute {
			return true
		}
	}
	return false
}

func runnerRegistered(runners []github.Runner, name string) bool {
	for _, r := range runners {
		if r.Name == name {
			return true
		}
	}
	return false
}
