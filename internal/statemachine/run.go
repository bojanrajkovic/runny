package statemachine

import (
	"context"
	"encoding/json"
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
	"github.com/bojanrajkovic/runny/internal/obs"
	"github.com/bojanrajkovic/runny/internal/tart"
	"github.com/bojanrajkovic/runny/internal/vm"
)

// run is one cycle's execution: the working state every state handler needs,
// created at cycle start, dead after teardown. It holds no *Slot — the
// slot-lifetime mutable state is the statusCell, a sibling dependency reached
// through cell. Contexts are never stored here; every method that needs one
// takes it as a parameter.
type run struct {
	cell  *statusCell
	deps  Deps // copied at creation
	cmds  <-chan Command
	name  string      // the slot's name, copied at creation
	store cycle.Store // built once (teardown and the audit sidecar share it)

	rec         *cycle.Record
	runnerName  string
	machine     vm.Machine
	guest       Guest
	proc        Proc
	runnerID    int64
	jobRan      bool
	arm         debugArm // today threaded runJob → watchJob → midJobInject as a param
	failState   State
	failErr     error
	vmDir       string
	runnerMount string
	srcBundle   tart.Bundle
}

// setStatus is the locked status-transition body shared by Slot.setState
// (BACKOFF, before any run exists) and run.setState (every other state) — the
// two differ only in which name/deps a cycle's identity comes from.
func setStatus(cell *statusCell, log *slog.Logger, name string, pool home.PoolConfig, state State, mut func(*Status)) {
	snap, fns := cell.update(func(st *Status) {
		st.Slot = name
		st.Pool = pool.Name   // slot-constant identity, like Slot
		st.Image = pool.Image // slot-constant identity, like Slot
		st.State = state
		st.StateEntered = time.Now()
		st.Detail = ""
		// Reset block: a debug hold's status belongs to exactly one state's
		// lifetime. Clearing both here means DebugHoldExpires/DebugHoldArmed
		// vanish the instant DEBUG or TEARDOWN (or any other state) is
		// entered; the mut below re-sets DebugHoldExpires when this
		// transition IS into DEBUG (issue #39).
		st.DebugHoldExpires = time.Time{}
		st.DebugHoldArmed = false
		if mut != nil {
			mut(st)
		}
	})
	log.Info("state", "state", state, "cycle", snap.CycleID)
	cell.notify(fns, snap)
}

func (c *run) setState(state State, mut func(*Status)) {
	setStatus(c.cell, c.deps.Log, c.name, c.deps.Pool, state, mut)
}

// beginStep opens one FSM step: publishes the state transition (with the
// completed-states snapshot) under mut, emits StepEntered, and opens the
// StateRecord. The returned ctx carries the step's obs scope; the returned
// finish stamps outcome/error, appends to rec.States, and emits StepLeft.
// finish is idempotent — a second call is a no-op, so a select-loop that
// calls it on every exit path never double-appends a StateRecord.
func (c *run) beginStep(ctx context.Context, state State, mut func(*Status)) (context.Context, func(cycle.Outcome, string)) {
	ctx = obs.WithStep(ctx, string(state))
	completed := slices.Clone(c.rec.States)
	c.setState(state, func(st *Status) {
		st.ActiveCycleStates = completed
		if mut != nil {
			mut(st)
		}
	})
	obs.Emit(ctx, obs.Event{Kind: obs.KindStepEntered, StepInfo: &obs.StepEvent{State: string(state)}})
	sr := cycle.StateRecord{State: string(state), Entered: time.Now()}
	var done bool
	finish := func(outcome cycle.Outcome, errStr string) {
		if done {
			return
		}
		done = true
		sr.Left, sr.Outcome, sr.Error = time.Now(), outcome, errStr
		c.rec.States = append(c.rec.States, sr)
		obs.Emit(ctx, obs.Event{Kind: obs.KindStepLeft, StepInfo: &obs.StepEvent{
			State: string(state), Outcome: obs.Outcome(outcome), Error: errStr,
		}})
	}
	return ctx, finish
}

// publish applies one learned fact to the cycle record and the live status
// under one lock acquisition, then emits its obs event and notifies watchers
// — one seam, three surfaces, so they can never disagree. recordOperatorKey
// shares this same lock-mutate-notify body directly (it has no obs event of
// its own — its callers already emit the audit trail events around it) so
// it isn't routed through publish, to avoid emitting a synthetic event.
func (c *run) publish(ctx context.Context, ev obs.Event, mut func(*cycle.Record, *Status)) {
	snap, fns := c.cell.update(func(st *Status) { mut(c.rec, st) })
	c.cell.notify(fns, snap)
	obs.Emit(ctx, ev)
}

// publishQuiet is publish without the notify: VM MAC and VM IP are each
// followed immediately by the next state's own setState broadcast, so an
// earlier notify here would be a redundant, benign extra one — unlike the
// RunnerVersion site (also silent today, on purpose), these have no such
// accepted-delta exception to spend.
func (c *run) publishQuiet(ctx context.Context, ev obs.Event, mut func(*cycle.Record, *Status)) {
	c.cell.update(func(st *Status) { mut(c.rec, st) })
	obs.Emit(ctx, ev)
}

// setDetail publishes a live annotation for the current state.
func (c *run) setDetail(ctx context.Context, detail string) {
	snap, fns, changed := c.cell.setDetailIfChanged(detail)
	if !changed {
		return
	}
	obs.Emit(ctx, obs.Event{Kind: obs.KindDetail, Detail: &obs.DetailEvent{Text: detail}})
	c.cell.notify(fns, snap)
}

// emitRunnerLine forwards one line of guest runner output to the configured
// sink (the runner log ring). The FSM's own reads are the tee points — a
// wrapper goroutine would block (and leak) on lines the FSM stops consuming
// after a marker ends its state; lines nobody reads simply aren't logged,
// matching what an operator could ever have observed.
func (c *run) emitRunnerLine(line string) {
	if c.deps.OnRunnerLine != nil {
		c.deps.OnRunnerLine(c.name, c.rec.CycleID, line)
	}
}

// runCycle executes states 1..9, always handing off to TEARDOWN, and returns
// the cycle record (teardown fills the tail), whether teardown wedged
// (force-stop failed with the guest still running), and whether the cycle
// ended benignly (operator recycle, daemon shutdown) rather than by failure.
func (c *run) runCycle(ctx context.Context) (*cycle.Record, bool, bool) {
	cfg := c.deps.Config

	// Operator commands must be able to interrupt ANY state, not just
	// LISTENING: a recycle issued mid-pull once sat queued for hours, then
	// fired on whatever healthy runner came up next. The watcher cancels the
	// cycle context with a typed cause; it hands command duty to LISTENING
	// (which has its own select) and is joined before that handoff so the
	// two never consume from cmds concurrently.
	cctx, ccancel := context.WithCancelCause(ctx)
	defer ccancel(nil)
	cctx = obs.WithCycle(cctx, c.deps.Events, obs.CycleRef{
		InstancePrefix: c.deps.InstancePrefix,
		Slot:           c.name,
		Pool:           c.deps.Pool.Name,
		Image:          c.deps.Pool.Image,
		CycleID:        c.rec.CycleID,
		RunnerName:     c.runnerName,
		Started:        c.rec.Started,
	})
	obs.Emit(cctx, obs.Event{Kind: obs.KindCycleStarted})
	stopWatch := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		for {
			select {
			case <-stopWatch:
				return
			case <-cctx.Done():
				return
			case cmd := <-c.cmds:
				switch cmd.Kind {
				case CmdRecycle:
					ccancel(fmt.Errorf("%w: %s", errOperatorRecycle, cmd.Reason))
				case CmdPause:
					c.cell.setPaused(true, cmd.ID)
				case CmdResume:
					c.cell.setPaused(false, cmd.ID)
				case CmdDebugKey:
					// Boot-path states have no usable hardened guest yet
					// (issue #39): reply and do nothing.
					if !cmd.Expires.IsZero() && time.Now().After(cmd.Expires) {
						cmd.reply(DebugKeyReply{Err: errors.New("command expired; nothing was injected")})
					} else {
						cmd.reply(DebugKeyReply{Err: errors.New("slot is mid-boot; key injection needs LISTENING, JOB, or DEBUG")})
					}
				}
			}
		}
	}()
	var stopOnce sync.Once
	stopWatcher := func() {
		stopOnce.Do(func() {
			close(stopWatch)
			<-watchDone
		})
	}
	defer stopWatcher()

	// ENSURE_IMAGE is the one state with no wall-clock deadline — pull
	// duration is unknowable — so it runs under the cycle context; its
	// operations carry their own bounds (resolve timeout, stall watcher).
	esctx := obs.WithStep(cctx, string(StateEnsureImage))
	ok := c.runState(StateEnsureImage, esctx, func() error {
		// The report callback fires on the shared image puller's own goroutine
		// (internal/images: every pull, shared or not, runs its own `go
		// p.run()`), not this cycle's FSM goroutine — an exception to "events
		// for one cycle are emitted from a single goroutine" (ADR-0024). Safe
		// regardless: obs's scope is immutable after WithStep/WithCycle and Seq
		// is atomic, so Detail events still land with a valid, unique Seq — just
		// not necessarily Time-ordered relative to this goroutine's other
		// events. The ensurer's own wait-for-pull action (correlated across
		// cycles by pull id) carries the proper shared-pull attribution.
		// The returned digest is deliberately dropped: the resolve callback
		// below already recorded it, at the moment it was learned.
		_, runnerVersion, bundle, err := c.deps.Images.Ensure(esctx, func(d string) { c.setDetail(esctx, d) }, func(d string) {
			// Fires as soon as the registry round-trip resolves the digest —
			// before the pull starts, synchronously on this goroutine.
			// Publish immediately so WatchStatus subscribers see the digest
			// mid-pull, not only at CLONE entry. The record write and the
			// image_info event sit together here so they can never disagree —
			// a cycle whose pull fails after resolve still records (and
			// emits) the digest it tried to pull.
			c.publish(esctx, obs.Event{Kind: obs.KindImageInfo, Image: &obs.ImageEvent{Digest: d}}, func(rec *cycle.Record, st *Status) {
				rec.ImageDigest = d
				st.ImageDigest = d
			})
		})
		if err != nil {
			return err
		}
		if runnerVersion != "" {
			// no resolver configured → no tarball, no event, and rec/status
			// already hold "" from cycle start (a fresh *cycle.Record, and
			// backoffWait resets Status.RunnerVersion before every cycle) —
			// nothing to publish when it's empty.
			//
			// Known accepted delta: this site used to skip notify (the next
			// setState, ENSURE_IMAGE → CLONE, broadcasts it milliseconds
			// later) — publish always notifies, so this now gains one extra,
			// benign status broadcast.
			c.publish(esctx, obs.Event{Kind: obs.KindImageInfo, Image: &obs.ImageEvent{RunnerVersion: runnerVersion}}, func(rec *cycle.Record, st *Status) {
				rec.RunnerVersion = runnerVersion
				st.RunnerVersion = runnerVersion
			})
		}
		c.srcBundle = bundle
		return nil
	})

	c.vmDir = c.deps.Home.VMDir(c.name)
	// runnerMount is the slot's own mount, or "" when this cycle resolved no
	// runner tarball (no resolver configured). One value drives both states:
	// CLONE populates it iff non-empty, BOOT mounts it iff non-empty (an empty
	// share dir attaches nothing), so the two can't disagree about whether a
	// tarball exists.
	if c.rec.RunnerVersion != "" {
		c.runnerMount = c.deps.Home.SlotRunnerMountDir(c.name)
	}
	if ok {
		ok = c.enter(cctx, StateClone, cfg.Deadlines.Clone.D(), func(bc bounded.Context) error {
			// A prior cycle's teardown removes vmDir best-effort; if that ever
			// failed, a surviving clone file (a bundle file or the runner
			// tarball) would make clonefile(2) below fail EEXIST every cycle,
			// wedging the slot in a CLONE→BACKOFF loop until a cold start. Clear
			// it first so CLONE is self-healing. Safe: no live guest owns vmDir
			// here — a wedged cycle parks the slot and never re-enters CLONE.
			if err := c.deps.RemoveAll(c.vmDir); err != nil {
				return fmt.Errorf("clearing stale clone dir: %w", err)
			}
			if err := c.deps.Clone(c.srcBundle, c.vmDir); err != nil {
				return err
			}
			if c.runnerMount == "" {
				return nil
			}
			// Give the cycle its own runner tarball: clone this cycle's
			// resolved version out of the shared store into the slot's mount.
			// From here the shared store is never read again, so no slot or GC
			// can disturb what this guest boots with.
			return cloneRunnerTarball(c.deps.CloneFile, c.deps.Home.RunnerCacheDir(), c.runnerMount, c.rec.RunnerVersion)
		})
	}
	if ok {
		ok = c.enter(cctx, StateBoot, cfg.Deadlines.Boot.D(), func(bc bounded.Context) error {
			m, err := c.deps.VM.Boot(bc, tart.Bundle(c.vmDir), vm.BootOptions{
				RunnerShareDir: c.runnerMount, // the slot's OWN mount, not the shared store
				CPUCount:       c.deps.Pool.CPUCores,
				MemorySize:     uint64(c.deps.Pool.RAMGB) << 30, // GiB → bytes; 0 keeps the image's
			})
			if err != nil {
				return err
			}
			c.machine = m
			mac := m.MAC()
			c.publishQuiet(bc, obs.Event{Kind: obs.KindVMInfo, VM: &obs.VMEvent{MAC: mac}}, func(rec *cycle.Record, st *Status) {
				rec.VM.MAC = mac
				st.VM.MAC = mac
			})
			return nil
		})
	}
	var ip string
	if ok {
		ok = c.enter(cctx, StateAwaitIP, cfg.Deadlines.AwaitIP.D(), func(bc bounded.Context) error {
			got, err := c.machine.WaitIP(bc)
			if err != nil {
				return err
			}
			ip = got
			c.publishQuiet(bc, obs.Event{Kind: obs.KindVMInfo, VM: &obs.VMEvent{IP: ip}}, func(rec *cycle.Record, st *Status) {
				rec.VM.IP = ip
				st.VM.IP = ip
			})
			return nil
		})
	}
	if ok {
		ok = c.enter(cctx, StateAwaitSSH, cfg.Deadlines.AwaitSSH.D(), func(bc bounded.Context) error {
			g, err := c.deps.Dial.WaitFor(bc, ip+":22", c.deps.Pool.OS)
			if err != nil {
				return err
			}
			c.guest = g
			return nil
		})
	}
	// SECURE_SSH must precede MINT_JIT, strictly: the JIT config — the
	// GitHub credential — must never exist while the guest still accepts
	// password auth. Only an explicit `off` skips the state; the
	// gate fails closed, so a zero-value SSHHardening (a Deps.Pool that never
	// saw config defaulting) rotates rather than silently un-hardening. With
	// `off`, the cycle record carries no SECURE_SSH entry and the cycle is
	// byte-identical to the pre-rotation daemon.
	if ok && c.deps.Pool.SSHHardening != home.SSHHardeningOff {
		ok = c.enter(cctx, StateSecureSSH, cfg.Deadlines.SecureSSH.D(), func(bc bounded.Context) error {
			return obs.Action(bc, obs.ActionRotate, func(context.Context) error {
				g, err := c.deps.Dial.Rotate(bc, ip+":22", c.guest, c.deps.Pool.OS)
				if err != nil {
					return err
				}
				c.guest = g
				return nil
			}, obs.Attr{Key: obs.AttrHardening, Value: string(c.deps.Pool.SSHHardening)})
		})
	}
	var jit *github.JITRunner
	if ok {
		ok = c.enter(cctx, StateMintJIT, cfg.Deadlines.MintJIT.D(), func(bc bounded.Context) error {
			j, err := c.deps.GitHub.GenerateJITConfig(bc, c.runnerName, c.deps.Pool.Labels, c.deps.Pool.RunnerGroupID)
			if err != nil {
				return err
			}
			jit = j
			c.runnerID = j.RunnerID
			obs.Emit(bc, obs.Event{Kind: obs.KindRunnerInfo, Runner: &obs.RunnerEvent{ID: j.RunnerID}})
			return nil
		})
	}
	if ok {
		ok = c.enter(cctx, StateProvision, cfg.Deadlines.Provision.D(), func(bc bounded.Context) error {
			// The proc must outlive this state's deadline: start it under the
			// cycle ctx, but bound the wait-for-listening here. The action
			// covers only the session start — the listening wait below is the
			// remainder of the step span's own time.
			err := obs.Action(bc, obs.ActionStartRunner, func(context.Context) error {
				p, err := c.guest.StartRunner(cctx, jit.EncodedJITConfig, c.deps.Pool.OS, c.rec.RunnerVersion)
				if err != nil {
					return err
				}
				c.proc = p
				return nil
			})
			if err != nil {
				return err
			}
			for {
				select {
				case <-bc.Done():
					return fmt.Errorf("runner did not reach %q: %w", markerListening, context.Cause(bc))
				case line, open := <-c.proc.Lines():
					if !open {
						code, _ := c.proc.Wait()
						return fmt.Errorf("runner exited (code %d) before listening", code)
					}
					c.emitRunnerLine(line)
					if strings.Contains(line, markerListening) {
						return nil
					}
				}
			}
		})
	}

	// LISTENING / JOB: watch-driven, no fixed deadline; budgets come from the
	// watches themselves. LISTENING services commands in its own select, so
	// the cycle watcher retires first.
	if ok {
		stopWatcher()
		c.jobRan, c.failState, c.failErr = c.listenAndRunJob(cctx)
		ok = c.failState == ""
	}

	// TEARDOWN — unconditional. Force is the floor; a guest that survives
	// even force-stop wedges the slot, and the record says so truthfully.
	stopWatcher()
	// The cycle's fate is sealed here — every state has returned and failErr
	// is final — so sample shutdown NOW. Teardown below runs detached for
	// tens of seconds (diag pull, stop escalation, clone removal); reading
	// ctx.Err() after it would relabel a real failure "shutdown" whenever the
	// daemon happens to stop mid-teardown, hiding the health signal and
	// exempting it from the streak.
	shutdown := ctx.Err() != nil
	wedged := c.teardown(cctx)

	// A cycle ended by operator recycle or daemon shutdown is recorded as a
	// failure (the timeline is truthful) but is benign for backoff: neither
	// says anything about the slot's health. A wedge is never benign. ending
	// names which of those two (or a plain failure) it was, for cycle.json.
	benign := false
	ending := cycle.EndingFailure
	if !ok {
		if cause := context.Cause(cctx); errors.Is(cause, errOperatorRecycle) {
			// Surface the recycle as the failure text instead of the bare
			// "context canceled" the interrupted state reported.
			c.failErr = cause
			benign = true
			ending = cycle.EndingRecycle
		} else if errors.Is(c.failErr, errOperatorRecycle) {
			benign = true
			ending = cycle.EndingRecycle
		} else if errors.Is(c.failErr, errDebugExpired) || errors.Is(c.failErr, errDebugRacedJob) {
			// A DEBUG hold that ran out, or a job that raced the LISTENING
			// freeze and died with the verified kill: operator-caused, not a
			// health signal (issue #39, §5.6). Not its own Ending class — it
			// still reads as "failure" (Result agrees); benign is what exempts
			// it from backoff. Checked before shutdown: the pre-teardown
			// snapshot already excludes a shutdown that lands during teardown,
			// so this ordering matters only when the daemon was stopping as
			// the hold expired (or the freeze kill finished) and the sentinel
			// still won the select — then the sentinel is the truthful, more
			// actionable label, and a "shutdown" ending would make `why`
			// suppress it.
			benign = true
		} else if shutdown {
			// Sampled before teardown: the daemon was already stopping when
			// the fate was sealed, so the failure is cancellation-shaped
			// (vendor seams don't reliably wrap context.Canceled, hence the
			// ambient check rather than an errors.Is on failErr).
			benign = true
			ending = cycle.EndingShutdown
		}
	}
	benign = benign && !wedged

	c.rec.Finished = time.Now()
	switch {
	case ok && !wedged:
		c.rec.Result = cycle.ResultSuccess
		ending = cycle.EndingSuccess
	case !ok:
		c.rec.Result = cycle.ResultFailure
		c.rec.Failure = &cycle.Failure{State: string(c.failState), Error: c.failErr.Error()}
	default: // the cycle succeeded but its teardown could not kill the guest
		c.rec.Result = cycle.ResultFailure
		c.rec.Failure = &cycle.Failure{State: string(StateTeardown), Error: "vm stop escalation failed; guest still running"}
	}
	if wedged {
		ending = cycle.EndingWedge
	}
	c.rec.Ending = ending

	finish := obs.FinishEvent{Result: string(c.rec.Result), Ending: string(c.rec.Ending)}
	if c.rec.Failure != nil {
		finish.FailureState, finish.Error = c.rec.Failure.State, c.rec.Failure.Error
	}
	obs.Emit(cctx, obs.Event{Kind: obs.KindCycleFinished, Finish: &finish})

	return c.rec, wedged, benign
}

// runState records one state's execution; sctx is consulted only to
// classify deadline outcomes. false = cycle failed.
func (c *run) runState(state State, sctx context.Context, f func() error) bool {
	_, finish := c.beginStep(sctx, state, func(st *Status) {
		st.CycleID = c.rec.CycleID
		st.RunnerName = c.runnerName
	})
	err := f()
	switch {
	case err == nil:
		finish(cycle.OutcomeOK, "")
	case errors.Is(err, context.DeadlineExceeded) || sctx.Err() != nil && errors.Is(context.Cause(sctx), context.DeadlineExceeded):
		finish(cycle.OutcomeDeadline, err.Error())
	default:
		finish(cycle.OutcomeError, err.Error())
	}
	if err != nil {
		c.failState, c.failErr = state, err
		return false
	}
	return true
}

// enter runs one deadline-bounded state. The state function receives the
// deadline as a bounded.Context it can hand straight to the guest and
// network seams — the per-state deadline is the contract, and the type
// system carries it to the call sites. The obs step scope is attached
// here too, so it rides the same bounded.Context into every seam.
func (c *run) enter(cctx context.Context, state State, d time.Duration, f func(bounded.Context) error) bool {
	sctx := obs.WithStep(cctx, string(state))
	bctx, cancel := bounded.WithTimeout(sctx, d)
	ok := c.runState(state, bctx, func() error { return f(bctx) })
	cancel()
	return ok
}

// listenAndRunJob handles LISTENING and JOB. Returns jobRan and, on failure,
// the failing state + error (empty state = clean completion). A DEBUG hold
// (issue #39) is reported as a benign failure carrying StateDebug.
func (c *run) listenAndRunJob(ctx context.Context) (bool, State, error) {
	cfg := c.deps.Config
	ctx, finishListening := c.beginStep(ctx, StateListening, func(st *Status) {
		// Reaching LISTENING proves the pre-boot failure (e.g. a doomed image
		// pull) is resolved. Clear the stale NOTE now rather than waiting for
		// finishCycle, so the status reflects "healthy" while the slot is healthy.
		// Both the published fields and the internal counter are reset under the
		// same lock acquisition so observers never see an inconsistent pair: this
		// closure runs inside setState's cell.update, so c.cell.mu is already held
		// here — a deliberate direct field touch, not an unlocked write.
		st.LastFailure = ""
		st.ConsecutiveFailures = 0
		c.cell.failures = 0
	})

	reconcile := time.NewTicker(cfg.Limits.ReconcileInterval.D())
	defer reconcile.Stop()
	maxIdle := time.NewTimer(cfg.Limits.MaxIdle.D())
	defer maxIdle.Stop()

	for {
		select {
		case <-ctx.Done():
			finishListening(cycle.OutcomeError, "daemon shutdown")
			return false, StateListening, ctx.Err()

		case cmd := <-c.cmds:
			switch cmd.Kind {
			case CmdRecycle:
				finishListening(cycle.OutcomeOK, "")
				return false, StateListening, fmt.Errorf("%w: %s", errOperatorRecycle, cmd.Reason)
			case CmdPause:
				c.cell.setPaused(true, cmd.ID) // takes effect at next BACKOFF
			case CmdResume:
				c.cell.setPaused(false, cmd.ID)
			case CmdDebugKey:
				// The LISTENING freeze (§5.1).
				done, racedLine, hstate, herr := c.freezeForDebug(ctx, cmd, finishListening)
				switch {
				case racedLine != "":
					// A job started before the request was serviced: fall into
					// the normal JOB transition with the buffered marker; the
					// freeze left the job untouched (decision 15).
					finishListening(cycle.OutcomeOK, "")
					return c.runJob(ctx, racedLine)
				case done:
					// The freeze committed (to DEBUG, or to a benign
					// raced-and-killed teardown) or refused fatally. The
					// raced-kill path is the one committed outcome where a job
					// actually ran: a runner picked up work during the freeze
					// and was killed, so GitHub considers it busy. jobRan=true
					// keeps teardown's !jobRan deregister gate from calling
					// DeleteRunner against that busy runner (§5.1 step 4, §16) —
					// the same coupling decision 2 rejected for eager-delete.
					return errors.Is(herr, errDebugRacedJob), hstate, herr
				}
				// Refused without committing (audit-write failure): keep
				// listening.
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
			runners, err := c.deps.GitHub.ListRunners(rctx)
			rcancel()
			if err != nil {
				// GitHub unreachable is transient: hold and log, never recycle
				// for it — sand's DNS-blip lesson.
				c.deps.Log.Warn("reconcile failed; holding", "err", err)
				continue
			}
			if !runnerRegistered(runners, c.runnerName) {
				finishListening(cycle.OutcomeError, "registration vanished")
				return false, StateListening, errors.New("zombie: registration vanished while listening")
			}

		case line, open := <-c.proc.Lines():
			if !open {
				code, _ := c.proc.Wait()
				finishListening(cycle.OutcomeError, fmt.Sprintf("runner exited (code %d) while idle", code))
				return false, StateListening, fmt.Errorf("runner exited (code %d) while listening", code)
			}
			c.emitRunnerLine(line)
			if !strings.Contains(line, markerJobStarted) {
				continue
			}
			finishListening(cycle.OutcomeOK, "")
			return c.runJob(ctx, line)
		}
	}
}

// runnerRegistered reports whether name appears among runners.
func runnerRegistered(runners []github.Runner, name string) bool {
	for _, r := range runners {
		if r.Name == name {
			return true
		}
	}
	return false
}

// maxJobName caps the guest-controlled job name. The name is sliced from the
// "Running job:" marker line, which is attacker-controlled and bounded only by
// maxLine (1 MB) at the reader; without this cap a 1 MB job name would land
// verbatim in cycle.json and in every WatchStatus snapshot's live Status.Job.
const maxJobName = 256

// jobNameFromMarker extracts the job name following the "Running job:" marker,
// trimmed and capped at maxJobName bytes on a valid-UTF-8 boundary (a naive
// byte slice could split a multibyte rune and write U+FFFD to disk/the wire).
func jobNameFromMarker(markerLine string) string {
	name := strings.TrimSpace(markerLine[strings.Index(markerLine, markerJobStarted)+len(markerJobStarted):])
	if len(name) > maxJobName {
		name = strings.ToValidUTF8(name[:maxJobName], "")
	}
	return name
}

// runJob drives JOB and, at job end, either tears down or enters a post-job
// DEBUG hold (issue #39). markerLine is the "Running job:" line. ctx is the
// CYCLE context (cctx): the job budget bounds only watchJob's jctx, never the
// operator's mid-job work (§3).
func (c *run) runJob(ctx context.Context, markerLine string) (bool, State, error) {
	cfg := c.deps.Config
	jobName := jobNameFromMarker(markerLine)
	job := &cycle.JobInfo{Name: jobName, Started: time.Now()}
	// rec.Job and status.Job are set together, inside beginStep's own locked
	// setState — not a separate publish call, so the JOB transition's single
	// notify already carries Job populated (a second, later publish call
	// here would fire an extra broadcast beyond the one documented delta).
	ctx, finish := c.beginStep(ctx, StateJob, func(st *Status) {
		c.rec.Job = job
		st.Job = job
	})
	obs.Emit(ctx, obs.Event{Kind: obs.KindJobStarted, Job: &obs.JobEvent{Name: jobName}})

	jctx, jcancel := context.WithTimeout(ctx, cfg.Limits.MaxJobDuration.D())
	jobOK, jobErr := c.watchJob(jctx, ctx)
	jcancel()

	var jobOutcome cycle.Outcome
	var jobErrStr string
	if jobOK {
		jobOutcome = cycle.OutcomeOK
	} else {
		jobOutcome, jobErrStr = cycle.OutcomeError, jobErr.Error()
	}
	// JobEnded before StepLeft (finish), mirroring JobStarted's position
	// after StepEntered: both bracket the JOB step from inside its span, so a
	// consumer never sees a job event for a step that's already closed.
	// c.rec.Job, not the local job pointer: recordOperatorKey is copy-on-write
	// and rebinds c.rec.Job to a fresh JobInfo per appended key — the original
	// this function captured never sees them.
	obs.Emit(ctx, obs.Event{Kind: obs.KindJobEnded, Job: &obs.JobEvent{
		Name: jobName, Outcome: obs.Outcome(jobOutcome), OperatorKeys: c.rec.Job.OperatorKeys,
	}})
	finish(jobOutcome, jobErrStr)

	switch {
	case !c.arm.armed:
		// Never armed, or disarmed mid-job: today's returns, verbatim.
	case errors.Is(jobErr, errOperatorRecycle):
		// Cancel consent killed the job: recycle means destroy, hold included.
		c.auditDisarm(ctx, "operator recycle canceled the job", slog.LevelInfo)
	case ctx.Err() != nil:
		// Daemon shutdown / cycle cancel: a hold would record a fake DEBUG
		// window.
		c.auditDisarm(ctx, "daemon shutdown", slog.LevelInfo)
	case c.cell.isPaused():
		// Decision 19 backstop (a re-arm after pause's disarm, or pause racing
		// the very last loop iteration): pause wins.
		c.auditDisarm(ctx, "slot paused", slog.LevelError)
	default:
		holdState, holdErr := c.enterPostJobDebug(ctx)
		if !jobOK {
			// The job's failure owns the cycle (§8): the DEBUG StateRecord is
			// on the record, but the failure is the job's. A failed post-job
			// hold (unproven kill) is recorded in the audit trail; surface it
			// to the daemon log too, since the cycle's failure is the job's.
			if holdErr != nil {
				c.deps.Log.Warn("post-job debug hold failed on a job that also failed; job error owns the cycle, hold failure is in the audit trail", "hold_err", holdErr)
			}
			return true, StateJob, jobErr
		}
		return true, holdState, holdErr
	}

	if jobOK {
		return true, "", nil
	}
	return true, StateJob, jobErr
}

func (c *run) watchJob(jctx, cctx context.Context) (bool, error) {
	for {
		select {
		case <-jctx.Done():
			// Budget expiry OR daemon shutdown (cctx). Drain-check FIRST
			// (closes the ~20s coin-flip window, §3): a completion marker or
			// channel-close buffered in Lines means the job COMPLETED near the
			// boundary, not a budget blowout.
			if done, ok := c.drainForCompletion(); done {
				return ok, nil
			}
			return false, fmt.Errorf("job exceeded budget: %w", context.Cause(jctx))

		case line, open := <-c.proc.Lines():
			if !open {
				// Ephemeral runners exit right after the job; treat exit
				// during JOB as completion-by-exit (the completion line
				// usually precedes it, but output can race the exit).
				code, _ := c.proc.Wait()
				if code == 0 {
					return true, nil
				}
				return false, fmt.Errorf("runner exited mid-job (code %d)", code)
			}
			c.emitRunnerLine(line)
			if strings.Contains(line, markerJobCompleted) {
				return true, nil
			}

		case cmd := <-c.cmds:
			switch cmd.Kind {
			case CmdPause:
				c.cell.setPaused(true, cmd.ID)
				if c.arm.armed { // disarm NOW + audit + clear status (decision 19)
					c.auditDisarm(cctx, "slot paused", slog.LevelError)
					c.clearArmedStatus(jobDetail(c.rec))
				}
			case CmdResume:
				c.cell.setPaused(false, cmd.ID)
			case CmdRecycle:
				if cmd.CancelJob {
					if c.arm.armed { // cancel consent destroys the job AND the armed hold
						c.auditDisarm(cctx, "operator recycle canceled the job", slog.LevelInfo)
						c.clearArmedStatus(jobDetail(c.rec))
					}
					return false, fmt.Errorf("%w: %s (running job canceled)", errOperatorRecycle, cmd.Reason)
				}
				if c.arm.armed { // plain recycle disarms + audit + clear status (§0)
					c.auditDisarm(cctx, "recycled without cancel consent", slog.LevelWarn)
					c.clearArmedStatus(jobDetail(c.rec))
				}
			case CmdDebugKey:
				c.midJobInject(cctx, cmd)
			}
		}
	}
}

// ---- debug-key injection (issue #39) ---------------------------------------

// armedKey records ONE mid-job install attempt and whether it provably landed
// (grep read-back ok). The `landed` bit is load-bearing: a retry of the same
// fingerprint after an AMBIGUOUS error must NOT take the exec-free re-arm path
// (decision 18's "an errored attempt never arms"). A naive []string would
// conflate landed and errored installs.
type armedKey struct {
	fingerprint string
	landed      bool // true iff InstallAuthorizedKey returned nil (read-back proven)
}

// debugArm is the per-cycle armed-hold state, owned by the FSM goroutine
// (never escapes runJob/watchJob; no locking). In-memory only.
type debugArm struct {
	armed bool
	hold  time.Duration // latest command wins
	keys  []armedKey    // attempted this job, with per-attempt landed outcome
}

func (a *debugArm) landed(fp string) bool {
	for _, k := range a.keys {
		if k.fingerprint == fp && k.landed {
			return true
		}
	}
	return false
}

// writeAuditSidecar writes the cycle's InjectedKeys to the operator-access.json
// sidecar (best-effort). Called after every audit mutation so the on-disk
// trail tracks memory; cycle.json itself lands only at finishCycle.
func (c *run) writeAuditSidecar() error {
	data, err := json.MarshalIndent(c.rec.InjectedKeys, "", "  ")
	if err != nil {
		return err
	}
	return c.store.WriteArtifact(c.rec, cycle.OperatorAccessFile, data)
}

// auditEvent mirrors one audit-trail entry into its observational obs copy,
// field for field — the event stream must never carry less than the record
// it shadows, or a trace's audit span events silently understate what
// cycle.json knows.
func auditEvent(k cycle.InjectedKey) *obs.AuditEvent {
	return &obs.AuditEvent{
		Fingerprint:  k.Fingerprint,
		Comment:      k.Comment,
		Reason:       k.Reason,
		Error:        k.Error,
		Outcome:      k.Outcome,
		State:        k.State,
		OperatorUID:  k.OperatorUID,
		OperatorUser: k.OperatorUser,
	}
}

// emitAuditAppend mirrors the entry just appended to c.rec.InjectedKeys into
// an AuditAppend event.
func (c *run) emitAuditAppend(ctx context.Context) {
	obs.Emit(ctx, obs.Event{Kind: obs.KindAuditAppend, Audit: auditEvent(c.rec.InjectedKeys[len(c.rec.InjectedKeys)-1])})
}

// appendPending appends a write-AHEAD "pending" audit entry and atomically
// writes the sidecar BEFORE any byte reaches the guest. The returned index
// addresses the entry for later updates. On write failure it removes the
// entry and returns ok=false: "no audit, no injection" (decision 4).
func (c *run) appendPending(ctx context.Context, cmd Command, state State) (int, bool) {
	c.rec.InjectedKeys = append(c.rec.InjectedKeys, cycle.InjectedKey{
		Fingerprint:  cmd.Fingerprint,
		Comment:      cmd.Comment,
		Injected:     time.Now(),
		Reason:       cmd.Reason,
		Outcome:      "pending",
		State:        string(state),
		OperatorUID:  cmd.OperatorUID,
		OperatorUser: cmd.OperatorUser,
	})
	idx := len(c.rec.InjectedKeys) - 1
	if err := c.writeAuditSidecar(); err != nil {
		c.rec.InjectedKeys = c.rec.InjectedKeys[:idx]
		c.deps.Log.Error("debug: write-ahead audit failed; injection refused", "err", err)
		return 0, false
	}
	c.emitAuditAppend(ctx)
	return idx, true
}

// updateAudit sets an entry's outcome/error and rewrites the sidecar
// (best-effort; the guest already changed, so a write failure only loses the
// on-disk copy until finishCycle rewrites from memory — decision 4). The obs
// event mirrors the in-memory entry regardless of the sidecar write's
// success: c.rec.InjectedKeys, not the sidecar, is cycle.json's eventual truth.
func (c *run) updateAudit(ctx context.Context, idx int, outcome, errStr string) {
	c.rec.InjectedKeys[idx].Outcome = outcome
	c.rec.InjectedKeys[idx].Error = errStr
	if err := c.writeAuditSidecar(); err != nil {
		c.deps.Log.Error("debug: post-exec audit rewrite failed", "err", err)
	}
	obs.Emit(ctx, obs.Event{Kind: obs.KindAuditUpdate, Audit: auditEvent(c.rec.InjectedKeys[idx])})
}

// auditDisarm appends a "disarmed" entry recording why an armed hold was
// cancelled without DEBUG entry, and rewrites the sidecar (best-effort).
func (c *run) auditDisarm(ctx context.Context, cause string, level slog.Level) {
	c.rec.InjectedKeys = append(c.rec.InjectedKeys, cycle.InjectedKey{
		Injected: time.Now(),
		Outcome:  "disarmed",
		Error:    cause,
		State:    string(StateJob),
	})
	if err := c.writeAuditSidecar(); err != nil {
		c.deps.Log.Error("debug: disarm audit rewrite failed", "err", err)
	}
	c.emitAuditAppend(ctx)
	c.deps.Log.Log(context.Background(), level, "debug hold disarmed", "cause", cause)
}

// recordOperatorKey appends fp to the cycle's job audit (JobInfo.OperatorKeys),
// copy-on-write under the lock. c.rec.Job and the cell's Status.Job alias one
// *JobInfo (set together at JOB entry), and Status()/the watch feed hand that
// pointer to gRPC goroutines — so an unlocked `append` to its slice is a live
// data race (a torn read of the slice header, or a read of a reallocated
// backing array). Replacing the JobInfo with a fresh copy + fresh slice leaves
// every snapshot a reader already holds immutable, and never mutates a slice
// another goroutine is reading. It then notifies watchers (like
// clearArmedStatus/setArmedStatus): the ambiguous install-failure caller
// records the possibly-live key and only rewrites the cycle sidecar
// afterward, so without a push here the fact that a privileged key may be on
// the guest stays invisible to StreamStatus until some unrelated status
// change fires.
func (c *run) recordOperatorKey(fp string) {
	snap, fns := c.cell.update(func(st *Status) {
		j := *c.rec.Job
		j.OperatorKeys = append(append([]string(nil), c.rec.Job.OperatorKeys...), fp)
		c.rec.Job = &j
		st.Job = &j
	})
	c.cell.notify(fns, snap)
}

// clearArmedStatus disarms in memory, clears Status.DebugHoldArmed, and
// rewrites Detail to the plain JOB line — used on every mid-job disarm (plain
// recycle, pause-while-armed). NOT setState (we are in JOB; StateEntered must
// not move). Mirrors the locked helper that SET the flag at arm time.
func (c *run) clearArmedStatus(detail string) {
	c.arm.armed = false
	snap, fns := c.cell.update(func(st *Status) {
		st.DebugHoldArmed = false
		st.Detail = detail
	})
	c.cell.notify(fns, snap)
}

// setArmedStatus sets Status.DebugHoldArmed and Detail at arm time (the locked
// mutate-and-notify mirror of clearArmedStatus; NOT setState).
func (c *run) setArmedStatus(detail string) {
	snap, fns := c.cell.update(func(st *Status) {
		st.DebugHoldArmed = true
		st.Detail = detail
	})
	c.cell.notify(fns, snap)
}

// jobDetail is the plain JOB status line (no armed annotation).
func jobDetail(rec *cycle.Record) string {
	if rec.Job != nil {
		return fmt.Sprintf("running job %q", rec.Job.Name)
	}
	return ""
}

// freezeForDebug runs the LISTENING freeze sequence (§5.1). Returns:
//   - racedLine != "": a job marker was seen; the caller transitions to JOB.
//   - done: the freeze committed (DEBUG hold entered, or a benign raced kill /
//     fatal refusal); hstate/herr carry the cycle outcome.
//   - neither: refused without committing (audit-write failure); keep listening.
func (c *run) freezeForDebug(ctx context.Context, cmd Command,
	finishListening func(cycle.Outcome, string),
) (done bool, racedLine string, hstate State, herr error) {
	secureSSH := c.deps.Config.Deadlines.SecureSSH.D()

	// 0. Guards.
	if !cmd.Expires.IsZero() && time.Now().After(cmd.Expires) {
		cmd.reply(DebugKeyReply{Err: errors.New("command expired; nothing was injected")})
		return false, "", "", nil
	}
	if cmd.CycleID != "" && cmd.CycleID != c.rec.CycleID {
		cmd.reply(DebugKeyReply{Err: fmt.Errorf("cycle %s already ended; nothing was injected", cmd.CycleID)})
		return false, "", "", nil
	}

	// 1. Write-ahead audit.
	idx, ok := c.appendPending(ctx, cmd, StateListening)
	if !ok {
		cmd.reply(DebugKeyReply{Err: errors.New("audit write failed; injection refused")})
		return false, "", "", nil
	}

	// 2. Drain-check for a raced job marker / idle runner-exit.
	if line, closed, marker := c.drainListeningLines(); marker {
		c.updateAudit(ctx, idx, "refused", "job started before service")
		cmd.reply(DebugKeyReply{Err: errors.New(
			"a job started before your request was serviced; nothing was injected — re-run debug to inject into the running job",
		)})
		return false, line, "", nil
	} else if closed {
		code, _ := c.proc.Wait()
		c.updateAudit(ctx, idx, "refused", "runner exited while idle")
		cmd.reply(DebugKeyReply{Err: errors.New("the runner exited before the key could be installed; nothing was injected")})
		finishListening(cycle.OutcomeError, fmt.Sprintf("runner exited (code %d) while idle", code))
		return true, "", StateListening, fmt.Errorf("runner exited (code %d) while listening", code)
	}

	// 3. Verified kill — before install, so the operator key never coexists
	// with a live (or ambiguously alive) runner.
	if err := c.boundedGuest(ctx, secureSSH, c.guest.StopRunner); err != nil {
		c.updateAudit(ctx, idx, "error", err.Error())
		cmd.reply(DebugKeyReply{Err: fmt.Errorf("could not verify the runner is dead; nothing was injected: %w", err)})
		finishListening(cycle.OutcomeError, "debug freeze: runner kill unproven")
		return true, "", StateListening, fmt.Errorf("debug freeze kill: %w", errDebugInjectFailed)
	}

	// 4. Drain Lines TO CLOSE (the process is proven dead), bounded 5s. A
	// marker here means a job raced and died with the kill — benign.
	if marker, closed := c.drainToClose(5 * time.Second); marker {
		c.rec.Job = &cycle.JobInfo{Name: "(raced the debug freeze)", Started: time.Now()}
		c.updateAudit(ctx, idx, "refused", "job raced the freeze and died with the kill")
		cmd.reply(DebugKeyReply{Err: errors.New("a job started and was killed by the freeze; nothing was injected")})
		finishListening(cycle.OutcomeOK, "")
		return true, "", StateJob, fmt.Errorf("%w", errDebugRacedJob)
	} else if !closed {
		c.updateAudit(ctx, idx, "error", "runner output did not close after kill")
		cmd.reply(DebugKeyReply{Err: errors.New("the runner did not close after the kill; nothing was injected")})
		finishListening(cycle.OutcomeError, "debug freeze: runner did not close")
		return true, "", StateListening, fmt.Errorf("debug freeze drain: %w", errDebugInjectFailed)
	}

	// 5. Install.
	if err := c.boundedGuest(ctx, secureSSH, func(bc bounded.Context) error {
		return c.guest.InstallAuthorizedKey(bc, cmd.PubKey)
	}); err != nil {
		c.updateAudit(ctx, idx, "error", err.Error())
		cmd.reply(DebugKeyReply{Err: fmt.Errorf("installing the key failed: %w", err)})
		finishListening(cycle.OutcomeError, "debug freeze: key install failed")
		return true, "", StateListening, fmt.Errorf("debug freeze install: %w", errDebugInjectFailed)
	}

	// 6. Success → DEBUG.
	c.updateAudit(ctx, idx, "ok", "")
	holdUntil := time.Now().Add(cmd.Hold)
	cmd.reply(DebugKeyReply{User: c.deps.Pool.SSHUser, HostKeys: c.guest.HostKeys(), HoldUntil: holdUntil})
	finishListening(cycle.OutcomeOK, "")
	st, err := c.holdForDebug(ctx, holdUntil)
	return true, "", st, err
}

// midJobInject installs an operator key into a RUNNING job's guest WITHOUT
// touching the runner, and arms a post-job DEBUG hold (§5.3). The job is never
// frozen or killed. ctx is the CYCLE context (cctx), not jctx (§3).
func (c *run) midJobInject(ctx context.Context, cmd Command) {
	secureSSH := c.deps.Config.Deadlines.SecureSSH.D()
	fp := cmd.Fingerprint

	// 0. Guards.
	if !cmd.Expires.IsZero() && time.Now().After(cmd.Expires) {
		cmd.reply(DebugKeyReply{Err: errors.New("command expired; nothing was injected")})
		return
	}
	if cmd.CycleID != "" && cmd.CycleID != c.rec.CycleID {
		cmd.reply(DebugKeyReply{Err: fmt.Errorf("cycle %s already ended; nothing was injected", cmd.CycleID)})
		return
	}
	if cmd.SeenState != StateJob {
		// A command aimed at a LISTENING slot that raced into JOB: refuse —
		// converting it into a mid-job injection would write contamination
		// into a CI job's permanent record without consent (decision 15).
		c.rec.InjectedKeys = append(c.rec.InjectedKeys, cycle.InjectedKey{
			Fingerprint: fp, Comment: cmd.Comment, Injected: time.Now(), Reason: cmd.Reason,
			Outcome: "refused", State: string(StateJob),
			Error:        "operator saw " + string(cmd.SeenState) + ", not JOB",
			OperatorUID:  cmd.OperatorUID,
			OperatorUser: cmd.OperatorUser,
		})
		_ = c.writeAuditSidecar()
		c.emitAuditAppend(ctx)
		cmd.reply(DebugKeyReply{Err: errors.New(
			"a job started before your request was serviced; nothing was injected — re-run debug to inject into the running job",
		)})
		return
	}

	// 1. RE-ARM (exec-free) ONLY for a PROVEN-LANDED key.
	if c.arm.landed(fp) {
		c.arm.hold = cmd.Hold
		c.rec.InjectedKeys = append(c.rec.InjectedKeys, cycle.InjectedKey{
			Fingerprint: fp, Comment: cmd.Comment, Injected: time.Now(), Reason: cmd.Reason,
			Outcome: "re-armed", State: string(StateJob),
			OperatorUID: cmd.OperatorUID, OperatorUser: cmd.OperatorUser,
		})
		_ = c.writeAuditSidecar()
		c.emitAuditAppend(ctx)
		c.setArmedStatus(c.armedDetail(fp, c.arm.hold))
		cmd.reply(c.armedReply())
		return
	}

	// 2. Write-ahead audit.
	idx, ok := c.appendPending(ctx, cmd, StateJob)
	if !ok {
		cmd.reply(DebugKeyReply{Err: errors.New("audit write failed; injection refused")})
		return
	}

	// 3. Install over the cycle's live session (a fresh channel; the runner
	// proc and the job are untouched). cctx, NOT jctx (§3).
	err := c.boundedGuest(ctx, secureSSH, func(bc bounded.Context) error {
		return c.guest.InstallAuthorizedKey(bc, cmd.PubKey)
	})
	switch {
	case err == nil:
		// 4. Success: arm.
		c.arm.armed = true
		c.arm.hold = cmd.Hold
		c.arm.keys = append(c.arm.keys, armedKey{fingerprint: fp, landed: true})
		c.recordOperatorKey(fp)
		c.updateAudit(ctx, idx, "armed", "")
		c.setArmedStatus(c.armedDetail(fp, c.arm.hold))
		cmd.reply(c.armedReply())
	case errors.Is(err, ErrGuestUnreachable):
		// NO Redial (decision 18); record not-landed so a retry re-proves.
		c.arm.keys = append(c.arm.keys, armedKey{fingerprint: fp, landed: false})
		c.updateAudit(ctx, idx, "unreachable", err.Error())
		cmd.reply(DebugKeyReply{Err: fmt.Errorf("guest session unreachable; nothing was injected and the job continues: %w", err)})
	default:
		// Ambiguous: the key may or may not have landed. Record contamination,
		// do NOT arm, the job continues (decision 18).
		c.arm.keys = append(c.arm.keys, armedKey{fingerprint: fp, landed: false})
		c.recordOperatorKey(fp)
		c.updateAudit(ctx, idx, "error", err.Error())
		cmd.reply(DebugKeyReply{Err: fmt.Errorf("install failed (key state unknown); the job continues: %w", err)})
	}
}

// armedReply is the mid-job armed reply: key live NOW, hold starts at job end
// so HoldUntil is zero (decision 16). The paused-while-arming warning
// (decision 19) is rendered by runnyctl from the slot's paused status, not
// carried here.
func (c *run) armedReply() DebugKeyReply {
	return DebugKeyReply{Armed: true, User: c.deps.Pool.SSHUser, HostKeys: c.guest.HostKeys()}
}

// armedDetail is the JOB+armed status line. hold is the operator's requested
// hold (the one enterPostJobDebug applies at job end), NOT the config cap — the
// live status must report the hold the slot will actually take.
func (c *run) armedDetail(fp string, hold time.Duration) string {
	return fmt.Sprintf("debug key installed (%s); holds %s at job end", fp, hold)
}

// enterPostJobDebug runs the post-job freeze tail (§5.4): verified kill, drain
// toward close (force-close tolerated), then the DEBUG hold. The clock starts
// at DEBUG entry (decision 16).
func (c *run) enterPostJobDebug(ctx context.Context) (State, error) {
	secureSSH := c.deps.Config.Deadlines.SecureSSH.D()

	// 1. Verified kill, always. At job end the kill prohibition has expired.
	if err := c.boundedGuest(ctx, secureSSH, c.guest.StopRunner); err != nil {
		c.rec.InjectedKeys = append(c.rec.InjectedKeys, cycle.InjectedKey{
			Injected: time.Now(), Outcome: "error", State: string(StateJob),
			Error: "post-job kill unproven: " + err.Error(),
		})
		_ = c.writeAuditSidecar()
		c.emitAuditAppend(ctx)
		// The hold is NOT entered — an unproven kill must not hold a
		// job-eligible guest. Clear the armed status now so DebugHoldArmed can't
		// linger into teardown.
		c.clearArmedStatus("post-job kill unproven; tearing down")
		return StateDebug, fmt.Errorf("debug hold entry: %w", errDebugInjectFailed)
	}

	// 2. Drain toward close, bounded 5s, force-close WITHOUT Wait on timeout
	// (the FSM-hang fix, §5.4 step 2).
	if _, closed := c.drainToClose(5 * time.Second); !closed {
		c.proc.Kill() // force-close the client-side channel; do NOT call Wait
		c.deps.Log.Debug("debug: post-job drain did not close; force-closed, exit code unknowable")
	}

	// 3. DEBUG hold; the clock starts NOW.
	holdUntil := time.Now().Add(c.arm.hold)
	return c.holdForDebug(ctx, holdUntil)
}

// holdForDebug is the frozen DEBUG loop (§5.5); both entry paths share it.
// max-idle is gone by construction; release is destruction.
func (c *run) holdForDebug(ctx context.Context, holdUntil time.Time) (State, error) {
	secureSSH := c.deps.Config.Deadlines.SecureSSH.D()
	ctx, finish := c.beginStep(ctx, StateDebug, func(st *Status) {
		st.DebugHoldExpires = holdUntil
		st.Detail = fmt.Sprintf("held for debug; release: runnyctl recycle %s", c.name)
	})

	hold := time.NewTimer(time.Until(holdUntil))
	defer hold.Stop()
	for {
		select {
		case <-ctx.Done():
			finish(cycle.OutcomeError, "daemon shutdown")
			return StateDebug, ctx.Err()
		case <-hold.C:
			finish(cycle.OutcomeOK, "")
			return StateDebug, fmt.Errorf("%w", errDebugExpired)
		case cmd := <-c.cmds:
			switch cmd.Kind {
			case CmdRecycle:
				finish(cycle.OutcomeOK, "")
				return StateDebug, fmt.Errorf("%w: %s", errOperatorRecycle, cmd.Reason)
			case CmdPause:
				c.cell.setPaused(true, cmd.ID) // holds in the NEXT BACKOFF; the hold itself untouched
			case CmdResume:
				c.cell.setPaused(false, cmd.ID)
			case CmdDebugKey:
				if ended := c.debugReArm(ctx, cmd, hold, secureSSH, finish); ended {
					return StateDebug, fmt.Errorf("debug hold install: %w", errDebugInjectFailed)
				}
				// still holding
			}
		}
	}
}

// debugReArm handles a CmdDebugKey dequeued in DEBUG (§5.5): re-arm an
// already-installed key exec-free, or install a new key. On a fatal install
// error it calls finish() and returns ended=true so the caller ends the hold.
func (c *run) debugReArm(ctx context.Context, cmd Command,
	hold *time.Timer, secureSSH time.Duration, finish func(cycle.Outcome, string),
) (ended bool) {
	if !cmd.Expires.IsZero() && time.Now().After(cmd.Expires) {
		cmd.reply(DebugKeyReply{Err: errors.New("command expired; nothing was injected")})
		return false
	}
	if cmd.CycleID != "" && cmd.CycleID != c.rec.CycleID {
		cmd.reply(DebugKeyReply{Err: fmt.Errorf("cycle %s already ended; nothing was injected", cmd.CycleID)})
		return false
	}
	// reset moves the auto-release deadline to now+cmd.Hold and publishes it,
	// returning the new deadline for the reply.
	reset := func() time.Time {
		newUntil := time.Now().Add(cmd.Hold)
		hold.Reset(time.Until(newUntil))
		snap, fns := c.cell.update(func(st *Status) { st.DebugHoldExpires = newUntil })
		c.cell.notify(fns, snap)
		return newUntil
	}

	// RE-ARM (exec-free): the fingerprint already has an ok/armed/re-armed
	// entry this cycle (survives a guest reboot).
	if c.keyInstalledThisCycle(cmd.Fingerprint) {
		newUntil := reset()
		c.rec.InjectedKeys = append(c.rec.InjectedKeys, cycle.InjectedKey{
			Fingerprint: cmd.Fingerprint, Comment: cmd.Comment, Injected: time.Now(), Reason: cmd.Reason,
			Outcome: "re-armed", State: string(StateDebug),
			OperatorUID: cmd.OperatorUID, OperatorUser: cmd.OperatorUser,
		})
		_ = c.writeAuditSidecar()
		c.emitAuditAppend(ctx)
		cmd.reply(DebugKeyReply{User: c.deps.Pool.SSHUser, HostKeys: c.guest.HostKeys(), HoldUntil: newUntil})
		return false
	}

	// NEW KEY: write-ahead, then install with a one-shot Redial retry.
	idx, ok := c.appendPending(ctx, cmd, StateDebug)
	if !ok {
		cmd.reply(DebugKeyReply{Err: errors.New("audit write failed; injection refused")})
		return false
	}
	install := func(bc bounded.Context) error { return c.guest.InstallAuthorizedKey(bc, cmd.PubKey) }
	err := c.boundedGuest(ctx, secureSSH, install)
	if errors.Is(err, ErrGuestUnreachable) {
		if rerr := c.boundedGuest(ctx, secureSSH, c.guest.Redial); rerr == nil {
			err = c.boundedGuest(ctx, secureSSH, install)
		}
	}
	switch {
	case err == nil:
		newUntil := reset()
		c.updateAudit(ctx, idx, "ok", "")
		cmd.reply(DebugKeyReply{User: c.deps.Pool.SSHUser, HostKeys: c.guest.HostKeys(), HoldUntil: newUntil})
		return false
	case errors.Is(err, ErrGuestUnreachable):
		c.updateAudit(ctx, idx, "unreachable", err.Error())
		cmd.reply(DebugKeyReply{Err: errors.New(
			"guest session is down (rebooted?); hold unchanged — extend with the already-installed key, or release with recycle",
		)})
		return false
	default:
		c.updateAudit(ctx, idx, "error", err.Error())
		cmd.reply(DebugKeyReply{Err: fmt.Errorf("installing the key failed: %w", err)})
		finish(cycle.OutcomeError, "debug hold: key install failed")
		return true
	}
}

// keyInstalledThisCycle reports whether fp has an ok/armed/re-armed audit
// entry this cycle — the DEBUG re-arm consults outcomes, not raw membership.
func (c *run) keyInstalledThisCycle(fp string) bool {
	for _, k := range c.rec.InjectedKeys {
		if k.Fingerprint == fp && keyOutcomeLanded(k.Outcome) {
			return true
		}
	}
	return false
}

// keyOutcomeLanded reports whether outcome means the key provably reached the
// guest. Used by keyInstalledThisCycle (exact match) and anyKeyLanded (below).
func keyOutcomeLanded(outcome string) bool {
	return outcome == "ok" || outcome == "armed" || outcome == "re-armed"
}

// anyKeyLanded reports whether a debug key may have landed this cycle —
// either proven (ok/armed/re-armed) or ambiguous (error: transport dropped
// after the authorized_keys write but before the grep read-back). The
// ambiguous case is included because PullDebugSession is safe to call when no
// file exists (the 2>/dev/null || true shell guard returns empty, which the
// len==0 check skips), so false negatives lose recordings and false positives
// cost one no-op SSH command.
func anyKeyLanded(rec *cycle.Record) bool {
	for _, k := range rec.InjectedKeys {
		if keyOutcomeLanded(k.Outcome) || k.Outcome == "error" {
			return true
		}
	}
	return false
}

// drainForCompletion non-blockingly drains c.proc.Lines after jctx fires: a
// buffered completion marker or a channel-close means the job COMPLETED near
// the budget boundary (§3 coin-flip fix). Returns done=true with the
// completion verdict, or done=false (a genuine blowout).
func (c *run) drainForCompletion() (done, ok bool) {
	for {
		select {
		case line, open := <-c.proc.Lines():
			if !open {
				code, _ := c.proc.Wait()
				return true, code == 0
			}
			c.emitRunnerLine(line)
			if strings.Contains(line, markerJobCompleted) {
				return true, true
			}
		default:
			return false, false
		}
	}
}

// drainListeningLines non-blockingly drains c.proc.Lines during the LISTENING
// freeze (§5.1 step 2), forwarding each line. Returns the job marker line (if
// any), whether the channel closed, and whether a marker was seen.
func (c *run) drainListeningLines() (jobLine string, closed, marker bool) {
	for {
		select {
		case line, open := <-c.proc.Lines():
			if !open {
				return "", true, false
			}
			c.emitRunnerLine(line)
			if strings.Contains(line, markerJobStarted) {
				return line, false, true
			}
		default:
			return "", false, false
		}
	}
}

// drainToClose drains c.proc.Lines toward close, bounded by d, forwarding
// lines. Returns whether a job marker appeared and whether the channel closed.
func (c *run) drainToClose(d time.Duration) (marker, closed bool) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case line, open := <-c.proc.Lines():
			if !open {
				return marker, true
			}
			c.emitRunnerLine(line)
			if strings.Contains(line, markerJobStarted) {
				marker = true
			}
		case <-timer.C:
			return marker, false
		}
	}
}

// boundedGuest calls a guest method that takes only a bounded.Context.
func (c *run) boundedGuest(ctx context.Context, d time.Duration, f func(bounded.Context) error) error {
	bctx, cancel := bounded.WithTimeout(ctx, d)
	defer cancel()
	return f(bctx)
}

// teardown is the universal sink. Post-mortem first (failure cycles), then
// stop → delete → deregister → record. Every step is best-effort with its own
// bound; nothing here can wedge the slot. Returns true when force-stop
// failed with the guest still running — the one failure teardown cannot
// absorb, because releasing an in-process VM takes a process exit.
func (c *run) teardown(ctx context.Context) bool {
	cfg := c.deps.Config
	ctx, finish := c.beginStep(ctx, StateTeardown, nil)
	// Teardown must run even when ctx (daemon shutdown) is done: detach.
	// context.WithoutCancel still forwards Value lookups to ctx, so the obs
	// scope survives detachment.
	tctx, cancel := bounded.WithTimeout(context.WithoutCancel(ctx), cfg.Deadlines.Teardown.D())
	defer cancel()

	failed := c.failState != ""
	debugKeyLanded := anyKeyLanded(c.rec)

	// Sub-steps below run inside obs.Action wrappers so a post-mortem trace
	// shows WHICH teardown sub-step degraded — cycle.json can only carry one
	// warn on the whole state. The returned errors are deliberately
	// discarded: the action event carries them, and teardown's own
	// best-effort accounting (logs, cleanupWarns) is unchanged. A skipped
	// sub-step emits no action at all.

	// 1. Post-mortem while the guest still exists (failure cycles only).
	if failed && c.guest != nil {
		pctx, pcancel := bounded.WithTimeout(tctx, 15*time.Second)
		defer pcancel()
		_ = obs.Action(ctx, obs.ActionDiagPull, func(context.Context) error {
			diag, err := c.guest.PullDiag(pctx)
			if err != nil {
				c.deps.Log.Debug("post-mortem pull failed", "err", err)
				return err
			}
			if len(diag) == 0 {
				return nil
			}
			// A pull that succeeded but whose artifact never landed must
			// not report ok — the operator would go looking for a
			// runner-diag.log that doesn't exist.
			dir, derr := c.store.Dir(c.rec)
			if derr != nil {
				c.deps.Log.Debug("post-mortem dir lookup failed", "err", derr)
				return derr
			}
			if werr := writeFile(dir, "runner-diag.log", diag); werr != nil {
				c.deps.Log.Debug("post-mortem write failed", "err", werr)
				return werr
			}
			c.rec.Artifacts = append(c.rec.Artifacts, "runner-diag.log")
			return nil
		})
	}

	// 1b. Debug session recording (if a debug key landed this cycle).
	// Gated on debug key landing, not on failure: a DEBUG hold expiry is benign.
	if debugKeyLanded && c.guest != nil {
		pctx, pcancel := bounded.WithTimeout(tctx, 5*time.Second)
		defer pcancel()
		_ = obs.Action(ctx, obs.ActionDebugSessionPull, func(context.Context) error {
			session, err := c.guest.PullDebugSession(pctx)
			if err != nil {
				c.deps.Log.Debug("debug session pull failed", "err", err)
				return err
			}
			if len(session) == 0 {
				return nil
			}
			dir, derr := c.store.Dir(c.rec)
			if derr != nil {
				c.deps.Log.Debug("debug session dir lookup failed", "err", derr)
				return derr
			}
			if werr := writeFile(dir, "debug-session.log", stripTerminalCodes(session)); werr != nil {
				c.deps.Log.Debug("debug session write failed", "err", werr)
				return werr
			}
			c.rec.Artifacts = append(c.rec.Artifacts, "debug-session.log")
			return nil
		})
	}

	// 2. Kill the runner proc and close the session.
	if c.proc != nil {
		c.proc.Kill()
	}
	if c.guest != nil {
		_ = c.guest.Close()
	}

	// 3. Stop the VM (graceful 10s → force; force is the floor).
	wedged := false
	var stopErr string
	if c.machine != nil {
		_ = obs.Action(ctx, obs.ActionStop, func(context.Context) error {
			err := c.machine.Stop(tctx, 10*time.Second)
			if err != nil {
				c.deps.Log.Error("vm stop escalation failed; guest still running", "err", err)
				wedged = true
				stopErr = fmt.Sprintf("vm stop escalation failed: %v", err)
			}
			return err
		})
	}

	// Steps 4 and 5 are best-effort cleanups: the guest is already destroyed, so
	// a failure here leaves only a swept-later orphan (a clone dir, an offline
	// registration). Recorded as a warn — never a bare ok (a clean-looking record
	// over a failed cleanup is the silent-record gap) and never an error (that is
	// the wedge: the mandatory teardown could not complete).
	var cleanupWarns []string

	// 4. Delete the clone bundle — unless the undead guest still holds it.
	// Deleting the disk out from under a live guest destroys the evidence
	// and frees nothing that matters (the guest-cap slot stays occupied).
	if !wedged {
		_ = obs.Action(ctx, obs.ActionCloneRemove, func(context.Context) error {
			err := c.deps.RemoveAll(c.vmDir)
			if err != nil {
				c.deps.Log.Error("removing vm dir", "err", err)
				cleanupWarns = append(cleanupWarns, fmt.Sprintf("remove clone: %v", err))
			}
			return err
		})
	}

	// 5. Deregister iff no job ran (JIT runners self-remove after a job).
	if c.runnerID != 0 && !c.jobRan {
		_ = obs.Action(ctx, obs.ActionDeregister, func(context.Context) error {
			err := c.deps.GitHub.DeleteRunner(tctx, c.runnerID)
			if err != nil {
				c.deps.Log.Warn("deregistering runner", "id", c.runnerID, "err", err)
				cleanupWarns = append(cleanupWarns, fmt.Sprintf("deregister runner %d: %v", c.runnerID, err))
			}
			return err
		})
	}

	var outcome cycle.Outcome
	errStr := stopErr
	switch {
	case wedged:
		// Recording OK here once hid the exact outage this project exists
		// to kill: cycle.json swore teardown succeeded while a ghost guest
		// ate the macOS guest cap and every later boot failed. A dereg can
		// still fail on this path (step 5 runs regardless); note it, but the
		// wedge dominates the outcome. errStr already holds the stop failure.
		outcome = cycle.OutcomeError
		if len(cleanupWarns) > 0 {
			errStr += "; " + strings.Join(cleanupWarns, "; ")
		}
	case len(cleanupWarns) > 0:
		outcome = cycle.OutcomeWarn
		errStr = strings.Join(cleanupWarns, "; ")
	default:
		outcome = cycle.OutcomeOK
	}
	finish(outcome, errStr)
	return wedged
}
