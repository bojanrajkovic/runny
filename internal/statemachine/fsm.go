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
	"strings"
	"sync"
	"time"

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

// GitHub is the slice of internal/github the FSM needs.
type GitHub interface {
	GenerateJITConfig(ctx context.Context, name string, labels []string, groupID int64) (*github.JITRunner, error)
	ListRunners(ctx context.Context) ([]github.Runner, error)
	DeleteRunner(ctx context.Context, id int64) error
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
	// run.sh with the JIT config.
	StartRunner(ctx context.Context, jit string) (Proc, error)
	// PullDiag fetches the tail of the runner's _diag logs (post-mortem).
	PullDiag(ctx context.Context) ([]byte, error)
	Close() error
}

// Dialer establishes Guest sessions (sshx's seam).
type Dialer interface {
	WaitFor(ctx context.Context, addr string) (Guest, error)
}

// Deps wires a slot to the world. Everything is an interface or func so the
// FSM tests on any OS with fakes.
type Deps struct {
	Home   home.Dir
	Config *home.Config
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
}

// Slot drives one runner slot's lifecycle.
type Slot struct {
	name string
	deps Deps

	cmds chan Command

	mu       sync.Mutex
	status   Status
	onChange func(Status)

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

// OnChange registers the status listener (the socket server's watch feed).
func (s *Slot) OnChange(fn func(Status)) {
	s.mu.Lock()
	s.onChange = fn
	s.mu.Unlock()
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
	fn := s.onChange
	s.mu.Unlock()
	s.deps.Log.Info("state", "state", state, "cycle", snap.CycleID)
	if fn != nil {
		fn(snap)
	}
}

// Run drives cycles until ctx is cancelled. This is the slot goroutine.
func (s *Slot) Run(ctx context.Context) {
	for {
		if err := s.backoffWait(ctx); err != nil {
			return // daemon shutdown
		}
		rec := s.runCycle(ctx)
		s.finishCycle(ctx, rec)
		if ctx.Err() != nil {
			return
		}
	}
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
	fn := s.onChange
	s.mu.Unlock()
	if fn != nil {
		fn(snap)
	}
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
	fn := s.onChange
	s.mu.Unlock()
	if fn != nil {
		fn(snap)
	}
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
// the cycle record (teardown fills the tail).
func (s *Slot) runCycle(ctx context.Context) *cycle.Record {
	cfg := s.deps.Config
	rec := &cycle.Record{
		CycleID: cycle.NewID(),
		Slot:    s.name,
		Started: time.Now(),
	}
	runnerName := fmt.Sprintf("%s-%s-%s", cfg.Runners.NamePrefix, s.name, rec.CycleID)

	var (
		machine   vm.Machine
		guest     Guest
		proc      Proc
		runnerID  int64
		jobRan    bool
		failState State
		failErr   error
	)

	// enter runs one deadline-bounded state; false = cycle failed.
	enter := func(state State, d time.Duration, f func(context.Context) error) bool {
		s.setState(state, func(st *Status) { st.CycleID = rec.CycleID })
		sr := cycle.StateRecord{State: string(state), Entered: time.Now()}
		sctx := ctx
		var cancel context.CancelFunc = func() {}
		if d > 0 {
			sctx, cancel = context.WithTimeout(ctx, d)
		}
		err := f(sctx)
		cancel()
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

	var srcBundle tart.Bundle
	ok := enter(StateEnsureImage, 0, func(c context.Context) error {
		// The puller carries its own progress-stall budget (Config.PullStall).
		digest, bundle, err := s.deps.Images.Ensure(c, s.setDetail)
		if err != nil {
			return err
		}
		rec.ImageDigest = digest
		srcBundle = bundle
		return nil
	})

	vmDir := s.deps.Home.VMDir(s.name)
	if ok {
		ok = enter(StateClone, cfg.Deadlines.Clone.D(), func(c context.Context) error {
			return s.deps.Clone(srcBundle, vmDir)
		})
	}
	if ok {
		ok = enter(StateBoot, cfg.Deadlines.Boot.D(), func(c context.Context) error {
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
		ok = enter(StateAwaitIP, cfg.Deadlines.AwaitIP.D(), func(c context.Context) error {
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
		ok = enter(StateAwaitSSH, cfg.Deadlines.AwaitSSH.D(), func(c context.Context) error {
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
		ok = enter(StateMintJIT, cfg.Deadlines.MintJIT.D(), func(c context.Context) error {
			j, err := s.deps.GitHub.GenerateJITConfig(c, runnerName, cfg.GitHub.Labels, cfg.GitHub.RunnerGroupID)
			if err != nil {
				return err
			}
			jit = j
			runnerID = j.RunnerID
			return nil
		})
	}
	if ok {
		ok = enter(StateProvision, cfg.Deadlines.Provision.D(), func(c context.Context) error {
			// The proc must outlive this state's deadline: start it under the
			// cycle ctx, but bound the wait-for-listening here.
			p, err := guest.StartRunner(ctx, jit.EncodedJITConfig)
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

	// TEARDOWN — unconditional, cannot fail (escalating force).
	s.teardown(ctx, rec, teardownInputs{
		machine:  machine,
		guest:    guest,
		proc:     proc,
		runnerID: runnerID,
		jobRan:   jobRan,
		vmDir:    vmDir,
		failed:   !ok,
	})

	rec.Finished = time.Now()
	if ok {
		rec.Result = cycle.ResultSuccess
	} else {
		rec.Result = cycle.ResultFailure
		rec.Failure = &cycle.Failure{State: string(failState), Error: failErr.Error()}
	}
	return rec
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
			runners, err := s.deps.GitHub.ListRunners(ctx)
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
// bound; nothing here can wedge the slot.
func (s *Slot) teardown(ctx context.Context, rec *cycle.Record, in teardownInputs) {
	cfg := s.deps.Config
	s.setState(StateTeardown, nil)
	tr := cycle.StateRecord{State: string(StateTeardown), Entered: time.Now()}
	// Teardown must run even when ctx (daemon shutdown) is done: detach.
	tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.Deadlines.Teardown.D())
	defer cancel()

	store := cycle.Store{SlotDir: s.deps.Home.SlotCyclesDir(s.name)}

	// 1. Post-mortem while the guest still exists (failure cycles only).
	if in.failed && in.guest != nil {
		pctx, pcancel := context.WithTimeout(tctx, 15*time.Second)
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
	if in.machine != nil {
		if err := in.machine.Stop(tctx, 10*time.Second); err != nil {
			s.deps.Log.Error("vm stop escalation failed", "err", err)
		}
	}

	// 4. Delete the clone bundle.
	if err := removeAll(in.vmDir); err != nil {
		s.deps.Log.Error("removing vm dir", "err", err)
	}

	// 5. Deregister iff no job ran (JIT runners self-remove after a job).
	if in.runnerID != 0 && !in.jobRan {
		if err := s.deps.GitHub.DeleteRunner(tctx, in.runnerID); err != nil {
			s.deps.Log.Warn("deregistering runner", "id", in.runnerID, "err", err)
		}
	}

	tr.Left, tr.Outcome = time.Now(), cycle.OutcomeOK
	rec.States = append(rec.States, tr)
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
