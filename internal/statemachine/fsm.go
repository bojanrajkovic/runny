// Package statemachine is runny's core: one crash-only FSM per runner slot.
// Every state is entered with a context deadline; the only
// response to any failure is TEARDOWN → BACKOFF with capped exponential
// backoff. Teardown cannot fail — escalating force is the floor.
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
	StateSecureSSH   State = "SECURE_SSH"
	StateMintJIT     State = "MINT_JIT"
	StateProvision   State = "PROVISION"
	StateListening   State = "LISTENING"
	StateJob         State = "JOB"
	StateTeardown    State = "TEARDOWN"
	StateDebug       State = "DEBUG"
)

// States lists every State the FSM can report. The proto-mapping
// exhaustiveness test keys off it; keep it in sync with the constants above
// (it sits directly below them so a new state is hard to miss).
var States = []State{
	StateBackoff, StateEnsureImage, StateClone, StateBoot, StateAwaitIP,
	StateAwaitSSH, StateSecureSSH, StateMintJIT, StateProvision,
	StateListening, StateJob, StateTeardown, StateDebug,
}

// Runner-output markers (the actions runner's run.sh wording).
const (
	markerListening    = "Listening for Jobs"
	markerJobStarted   = "Running job:"
	markerJobCompleted = "completed with result:"
)

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

// errOperatorRecycle marks a cycle ended on purpose by `runnyctl recycle`.
// Cycles it ends are recorded as failures (the timeline is truthful) but are
// benign for backoff accounting: an operator action is not a health signal.
var errOperatorRecycle = errors.New("recycled by operator")

// Debug-key injection sentinels (issue #39).
var (
	// errDebugExpired: a DEBUG hold ran out — benign for backoff.
	errDebugExpired = errors.New("debug hold expired")
	// errDebugRacedJob: a job was assigned during the LISTENING freeze and
	// died with the verified kill — benign, operator-caused.
	errDebugRacedJob = errors.New("job raced the debug freeze")
	// errDebugInjectFailed: a freeze or hold-entry touched the guest and
	// failed, or death could not be proven — this counts toward the streak.
	errDebugInjectFailed = errors.New("debug key injection failed")
	// ErrGuestUnreachable: the guest seam proved the command never reached
	// the guest (a session-open failure). internal/guest wraps sshx's
	// ErrSessionOpen with it; mid-job, it means "nothing was sent" so the job
	// is untouched and the slot is not redialed (decision 18).
	ErrGuestUnreachable = errors.New("guest unreachable")
)

// ImageEnsurer makes sure the configured image is cached locally and returns
// its digest and bundle dir (ENSURE_IMAGE's work). report receives live
// progress annotations ("2.1 GiB at 41 MiB/s") — pull progress must be
// visible, not just stall-detected, so an operator can tell slow-registry
// from stuck (the predecessor made them indistinguishable).
type ImageEnsurer interface {
	Ensure(ctx context.Context, report func(detail string), onDigestResolved func(string)) (digest, runnerVersion string, bundle tart.Bundle, err error)
}

// Cloner clones a bundle (tart.Clone's seam).
type Cloner func(src tart.Bundle, dst string) error

// GitHub is the slice of internal/github the FSM needs. Every method takes
// bounded.Context: these are network calls, and an unbounded call site is a
// compile error.
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
	// run.sh with the JIT config; goos selects the per-OS provision path and
	// runnerTarball is the exact tarball basename to stage (this cycle's
	// resolved RunnerVersion), not a glob. It deliberately takes a plain
	// context: the ctx is the proc's LIFETIME — the whole cycle, outliving
	// PROVISION's deadline, cancelled by operator recycle or daemon shutdown —
	// not an operation bound; session establishment is bounded internally by
	// sshx's socket deadlines.
	StartRunner(ctx context.Context, jit, goos, runnerTarball string) (Proc, error)
	// PullDiag fetches the tail of the runner's _diag logs (post-mortem).
	PullDiag(ctx bounded.Context) ([]byte, error)
	// StopRunner kills the runner listener tree and PROVES it dead (a pgrep
	// read-back loop). Any nonzero exit or exec error = death unproven, and
	// the caller refuses the freeze/hold (issue #39, decision 2).
	StopRunner(ctx bounded.Context) error
	// InstallAuthorizedKey appends one authorized_keys line and proves it
	// landed (a grep read-back). A session-open failure is wrapped with
	// ErrGuestUnreachable (the command provably never reached the guest).
	InstallAuthorizedKey(ctx bounded.Context, line string) error
	// HostKeys returns the pinned guest host keys in known_hosts form; empty
	// when the guest is unhardened (ssh_hardening: off).
	HostKeys() []string
	// Redial re-establishes the session after transport death (a guest
	// reboot). NEVER called during JOB — it closes the client carrying the
	// running proc (decision 18).
	Redial(ctx bounded.Context) error
	Close() error
}

// Dialer establishes Guest sessions (sshx's seam).
type Dialer interface {
	WaitFor(ctx bounded.Context, addr string) (Guest, error)
	// Rotate hardens an authenticated session (the SECURE_SSH
	// state): mint a per-cycle key, install it in the guest, disable
	// password auth, reconnect keyed with host keys pinned. Returns the new
	// session; on success the old one is closed, on failure the caller
	// still owns g (teardown pulls diag over it, then closes it). goos
	// selects the per-OS rotation script.
	Rotate(ctx bounded.Context, addr string, g Guest, goos string) (Guest, error)
}

// Deps wires a slot to the world. Everything is an interface or func so the
// FSM tests on any OS with fakes. Images/GitHub/Dial are pool-scoped
// (per-pool image, registration target, and guest credentials);
// Config carries the global deadlines/limits/retention.
type Deps struct {
	Home   home.Dir
	Config *home.Config
	Pool   home.PoolConfig
	// InstancePrefix is this install's runner-name namespace
	// (<slug(hostname)>-<rand8>, derived and persisted by home.Dir):
	// runner names are <InstancePrefix>-<slot>-<cycle8>.
	InstancePrefix string
	VM             vm.Manager
	Images         ImageEnsurer
	Clone          Cloner
	GitHub         GitHub
	Dial           Dialer
	Log            *slog.Logger
	// OnRunnerLine, when set, receives every line of the guest runner's
	// output (run.sh stdout/stderr) as it arrives — the feed for the
	// runnyctl-visible runner log stream. Called on the FSM's line path; it
	// must not block (sink into a logring.Ring, whose fan-out drops on slow
	// subscribers).
	OnRunnerLine func(slot, cycleID, line string)
}

// Command is an operator injection (from runnyctl via the socket).
type Command struct {
	Kind   CommandKind
	Reason string
	// ID is the client's opaque correlation id for CmdPause/CmdResume (a
	// random UUID from PauseRequest/ResumeRequest.command_id). setPaused
	// appends it to Status.RecentAppliedCommandIDs when the command applies,
	// so the client can confirm THIS command by membership. Empty for
	// daemon-internal re-issues (the drainer) and older clients; an empty id
	// is never recorded (see setPaused).
	ID string
	// CancelJob applies to CmdRecycle only: consent to cancel a RUNNING job
	// (decision 14). runnyctl sets it via its -force guard after observing
	// JOB. Without it, a mid-job recycle disarms any debug hold and the job
	// runs to its normal end.
	CancelJob bool
	// The fields below apply to CmdDebugKey only (issue #39).
	PubKey      string             // canonical authorized_keys line (re-marshaled, shell-safe)
	Fingerprint string             // SHA256:… (computed server-side)
	Comment     string             // submitted key's comment, audit only — never reaches the guest
	Hold        time.Duration      // validated server-side; <=0 never reaches the FSM
	CycleID     string             // the cycle the operator saw; consumers reject a mismatch
	SeenState   State              // the state the operator saw (consent pin, decision 15)
	Expires     time.Time          // enqueue + queueBound; consumers reject a late dequeue
	Reply       chan DebugKeyReply // buffered 1; replied via select/default
}

type CommandKind int

const (
	CmdRecycle CommandKind = iota
	CmdPause
	CmdResume
	CmdDebugKey
)

// DebugKeyReply is the synchronous answer to a CmdDebugKey, sent back over the
// command's buffered Reply channel (issue #39).
type DebugKeyReply struct {
	Err       error
	User      string
	HostKeys  []string
	HoldUntil time.Time // zero when Armed
	Armed     bool      // mid-job install: key live NOW, hold starts at job end
}

// reply sends r over the command's buffered (size-1) Reply channel without
// ever blocking: a CmdDebugKey whose handler already gave up (timeout) leaves
// nobody reading, and the FSM goroutine must never wedge on it.
func (c Command) reply(r DebugKeyReply) {
	if c.Reply == nil {
		return
	}
	select {
	case c.Reply <- r:
	default:
	}
}

// Status is the live snapshot the control surface renders.
type Status struct {
	Slot         string
	State        State
	StateEntered time.Time
	CycleID      string
	// RunnerName is the GitHub-visible name of the current cycle's runner
	// (<prefix>-<slot>-<cycle8>); empty in BACKOFF, when no runner exists.
	RunnerName string
	// Image is the pool's configured image ref (config.yaml `image`,
	// verbatim); constant for the slot's lifetime.
	Image string
	// ImageDigest is the digest resolved by the current cycle's
	// ENSURE_IMAGE ("sha256:..."); empty before resolve and in BACKOFF.
	// Presence = resolved this cycle (the cycle may still fail before
	// boot). Retained while wedged: the surviving guest still runs it.
	ImageDigest string
	// RunnerVersion is the asset filename of the actions-runner tarball
	// ensured this cycle (e.g. "actions-runner-osx-arm64-2.320.0.tar.gz");
	// empty before ENSURE_IMAGE completes and in BACKOFF.
	RunnerVersion string
	Paused        bool
	// RecentAppliedCommandIDs is a bounded, oldest-evicted history of the
	// IDENTIFIED pause/resume Command.IDs the FSM has applied for this slot. The
	// control surface confirms a pending pause/resume by membership, so
	// concurrent clients don't clobber each other's acknowledgement the way a
	// single last-applied scalar would. An id-less internal re-issue appends
	// nothing, so a client's id persists across unrelated status snapshots.
	RecentAppliedCommandIDs []string
	ConsecutiveFailures     uint32
	BackoffSeconds          int64
	VM                      cycle.VMInfo
	Job                     *cycle.JobInfo
	LastFailure             string
	// Detail is the current state's live annotation (pull progress etc).
	Detail string
	// Wedged: the guest survived force-stop and still occupies a
	// Virtualization.framework guest slot. The slot is parked; only a daemon
	// restart (cold start) reclaims the guest.
	Wedged bool
	// DebugHoldExpires is the auto-release deadline of a DEBUG hold; non-zero
	// only in DEBUG, cleared the instant DEBUG is left (issue #39).
	DebugHoldExpires time.Time
	// DebugHoldArmed is true iff state == JOB with a debug hold currently
	// armed: the slot will enter DEBUG (not teardown) at job end. Cleared the
	// instant the hold is disarmed or DEBUG/TEARDOWN is entered (issue #39).
	DebugHoldArmed bool
	// ActiveCycleStates is the ordered list of states that have already
	// completed in the current in-flight cycle. Populated on each state
	// transition (the new-state snapshot carries all prior completed states);
	// cleared when the slot enters BACKOFF. The current state is State +
	// StateEntered — it is not included here.
	ActiveCycleStates []cycle.StateRecord
}

// Slot drives one runner slot's lifecycle.
type Slot struct {
	name string
	deps Deps

	cmds chan Command

	mu       sync.Mutex
	status   Status
	onChange []func(Status)

	// failure streak for backoff; reset on job completion or a held LISTENING.
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
		// Slot and Image are slot-constant identity (the name and the configured
		// ref), not cycle state. Seeded here so a slot that hasn't transitioned
		// yet still renders a complete row, and re-set on every transition by
		// setState — so neither depends on this seed surviving a future
		// struct-replace refactor.
		status: Status{Slot: name, Image: deps.Pool.Image},
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
// the slot's lock, and synchronously on FSM goroutines — they must not
// block (fan out through a buffered channel like the socket server does).
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

// Name returns the slot's immutable name (the status snapshot's Slot field
// is empty until the first state transition; lookups must not depend on it).
func (s *Slot) Name() string { return s.name }

// Status returns the current snapshot.
func (s *Slot) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

func (s *Slot) setState(state State, mut func(*Status)) {
	s.mu.Lock()
	s.status.Slot = s.name
	s.status.Image = s.deps.Pool.Image // slot-constant identity, like Slot
	s.status.State = state
	s.status.StateEntered = time.Now()
	s.status.Detail = ""
	// Reset block: a debug hold's status belongs to exactly one state's
	// lifetime. Clearing both here means DebugHoldExpires/DebugHoldArmed
	// vanish the instant DEBUG or TEARDOWN (or any other state) is entered;
	// the mut below re-sets DebugHoldExpires when this transition IS into
	// DEBUG (issue #39).
	s.status.DebugHoldExpires = time.Time{}
	s.status.DebugHoldArmed = false
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
		rec, wedged, benign := s.runCycle(ctx)
		s.finishCycle(rec, benign)
		if wedged {
			// The guest survived force-stop: it still occupies one of the
			// host's Virtualization.framework guest slots, so every further
			// boot on this slot is doomed. Park; only a daemon restart (cold
			// start) reclaims an in-process VM.
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
		st.RunnerName = "" // the cycle's runner no longer exists
		// No guest exists; stale values after an image/runner bump are misinformation.
		st.ImageDigest = ""
		st.RunnerVersion = ""
		st.VM = cycle.VMInfo{}
		st.Job = nil
		st.ActiveCycleStates = nil
	})
	// Drain any command stranded by the previous cycle's TEARDOWN (the one
	// consumer-less window) BEFORE arming the timer. A plain CmdRecycle left
	// here would otherwise race a zero backoff timer and reach this cycle's
	// runCycle watcher, canceling a healthy boot (the stale-recycle class,
	// §0); discarding it makes "recycle a slot that no longer exists" the
	// no-op handleIdleCommand already treats it as. A CmdDebugKey is replied
	// "expired"; pause/resume are idempotent and applied (issue #39).
drain:
	for {
		select {
		case cmd := <-s.cmds:
			s.handleIdleCommand(cmd)
		default:
			break drain
		}
	}
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
		s.setPaused(true, cmd.ID)
	case CmdResume:
		s.setPaused(false, cmd.ID)
	case CmdRecycle:
		// Nothing to recycle while idle.
	case CmdDebugKey:
		// No guest exists in BACKOFF. An expired command (the common stranded
		// case) reads as expired; an unexpired one as the precise reason.
		if !cmd.Expires.IsZero() && time.Now().After(cmd.Expires) {
			cmd.reply(DebugKeyReply{Err: errors.New("command expired; nothing was injected")})
			return
		}
		cmd.reply(DebugKeyReply{Err: errors.New("no guest exists in BACKOFF; key injection needs LISTENING, JOB, or DEBUG")})
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

// recentCommandIDCap bounds the per-slot applied-command history. Generous
// relative to the client's confirm→catch window: an id only needs to survive
// from the snapshot that carries it until the client polls, so a handful is
// plenty, and the cap exists only to keep an always-paused slot's history from
// growing without bound.
const recentCommandIDCap = 16

// setPaused applies a pause/resume and republishes the slot status. cmdID is
// the applying command's correlation id (empty for daemon-internal re-issues);
// a non-empty id is appended to Status.RecentAppliedCommandIDs so the control
// surface can confirm that specific command by membership. An empty cmdID
// appends nothing, so a coalesced status stream never drops a client's
// acknowledgement out of the history.
func (s *Slot) setPaused(p bool, cmdID string) {
	s.mu.Lock()
	s.paused = p
	s.status.Paused = p
	if cmdID != "" {
		s.status.RecentAppliedCommandIDs = appendBounded(
			s.status.RecentAppliedCommandIDs, cmdID, recentCommandIDCap,
		)
	}
	snap := s.status
	fns := slices.Clone(s.onChange)
	s.mu.Unlock()
	s.notify(fns, snap)
}

// appendBounded returns ids with id appended, retaining at most max entries
// (oldest evicted). It always allocates a fresh backing array so a status value
// already snapshotted under the lock keeps its own stable slice — a later
// append must not mutate an array a prior snapshot still references.
func appendBounded(ids []string, id string, max int) []string {
	next := make([]string, 0, max)
	if len(ids) >= max {
		next = append(next, ids[len(ids)-max+1:]...)
	} else {
		next = append(next, ids...)
	}
	return append(next, id)
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

// runCycle executes states 1..9, always handing off to TEARDOWN, and returns
// the cycle record (teardown fills the tail), whether teardown wedged
// (force-stop failed with the guest still running), and whether the cycle
// ended benignly (operator recycle, daemon shutdown) rather than by failure.
func (s *Slot) runCycle(ctx context.Context) (*cycle.Record, bool, bool) {
	cfg := s.deps.Config
	rec := &cycle.Record{
		CycleID: cycle.NewID(),
		Slot:    s.name,
		Image:   s.deps.Pool.Image, // intent recorded at cycle start, before any state runs
		Started: time.Now(),
	}
	runnerName := fmt.Sprintf("%s-%s-%s", s.deps.InstancePrefix, s.name, rec.CycleID)

	// Operator commands must be able to interrupt ANY state, not just
	// LISTENING: a recycle issued mid-pull once sat queued for hours, then
	// fired on whatever healthy runner came up next. The watcher cancels the
	// cycle context with a typed cause; it hands command duty to LISTENING
	// (which has its own select) and is joined before that handoff so the
	// two never consume from cmds concurrently.
	cctx, ccancel := context.WithCancelCause(ctx)
	defer ccancel(nil)
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
			case cmd := <-s.cmds:
				switch cmd.Kind {
				case CmdRecycle:
					ccancel(fmt.Errorf("%w: %s", errOperatorRecycle, cmd.Reason))
				case CmdPause:
					s.setPaused(true, cmd.ID)
				case CmdResume:
					s.setPaused(false, cmd.ID)
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
		completed := slices.Clone(rec.States)
		s.setState(state, func(st *Status) {
			st.CycleID = rec.CycleID
			st.RunnerName = runnerName
			st.ActiveCycleStates = completed
		})
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
	// system carries it to the call sites.
	enter := func(state State, d time.Duration, f func(bounded.Context) error) bool {
		bctx, cancel := bounded.WithTimeout(cctx, d)
		ok := runState(state, bctx, func() error { return f(bctx) })
		cancel()
		return ok
	}

	var srcBundle tart.Bundle
	// ENSURE_IMAGE is the one state with no wall-clock deadline — pull
	// duration is unknowable — so it runs under the cycle context; its
	// operations carry their own bounds (resolve timeout, stall watcher).
	ok := runState(StateEnsureImage, cctx, func() error {
		digest, runnerVersion, bundle, err := s.deps.Images.Ensure(cctx, s.setDetail, func(d string) {
			// Fires as soon as the registry round-trip resolves the digest —
			// before the pull starts. Publish immediately so WatchStatus
			// subscribers see the digest mid-pull, not only at CLONE entry.
			s.mu.Lock()
			s.status.ImageDigest = d
			snap := s.status
			fns := slices.Clone(s.onChange)
			s.mu.Unlock()
			s.notify(fns, snap)
		})
		if err != nil {
			return err
		}
		rec.ImageDigest = digest
		rec.RunnerVersion = runnerVersion
		// RunnerVersion: no explicit notify needed — the next setState
		// (ENSURE_IMAGE → CLONE) broadcasts it milliseconds later.
		s.mu.Lock()
		s.status.RunnerVersion = runnerVersion
		s.mu.Unlock()
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
				CPUCount:       s.deps.Pool.CPUCores,
				MemorySize:     uint64(s.deps.Pool.RAMGB) << 30, // GiB → bytes; 0 keeps the image's
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
	// SECURE_SSH must precede MINT_JIT, strictly: the JIT config — the
	// GitHub credential — must never exist while the guest still accepts
	// password auth. Only an explicit `off` skips the state; the
	// gate fails closed, so a zero-value SSHHardening (a Deps.Pool that never
	// saw config defaulting) rotates rather than silently un-hardening. With
	// `off`, the cycle record carries no SECURE_SSH entry and the cycle is
	// byte-identical to the pre-rotation daemon.
	if ok && s.deps.Pool.SSHHardening != home.SSHHardeningOff {
		ok = enter(StateSecureSSH, cfg.Deadlines.SecureSSH.D(), func(c bounded.Context) error {
			g, err := s.deps.Dial.Rotate(c, ip+":22", guest, s.deps.Pool.OS)
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
			p, err := guest.StartRunner(cctx, jit.EncodedJITConfig, s.deps.Pool.OS, rec.RunnerVersion)
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
					s.emitRunnerLine(rec.CycleID, line)
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
		jobRan, failState, failErr = s.listenAndRunJob(cctx, rec, proc, guest, runnerName)
		ok = failState == ""
	}

	// TEARDOWN — unconditional. Force is the floor; a guest that survives
	// even force-stop wedges the slot, and the record says so truthfully.
	stopWatcher()
	wedged := s.teardown(cctx, rec, teardownInputs{
		machine:  machine,
		guest:    guest,
		proc:     proc,
		runnerID: runnerID,
		jobRan:   jobRan,
		vmDir:    vmDir,
		failed:   !ok,
	})

	// A cycle ended by operator recycle or daemon shutdown is recorded as a
	// failure (the timeline is truthful) but is benign for backoff: neither
	// says anything about the slot's health. A wedge is never benign.
	benign := false
	if !ok {
		if cause := context.Cause(cctx); errors.Is(cause, errOperatorRecycle) {
			// Surface the recycle as the failure text instead of the bare
			// "context canceled" the interrupted state reported.
			failErr = cause
			benign = true
		} else if errors.Is(failErr, errOperatorRecycle) || ctx.Err() != nil {
			benign = true
		} else if errors.Is(failErr, errDebugExpired) || errors.Is(failErr, errDebugRacedJob) {
			// A DEBUG hold that ran out, or a job that raced the LISTENING
			// freeze and died with the verified kill: operator-caused, not a
			// health signal (issue #39, §5.6).
			benign = true
		}
	}
	benign = benign && !wedged

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
	return rec, wedged, benign
}

// listenAndRunJob handles LISTENING and JOB. Returns jobRan and, on failure,
// the failing state + error (empty state = clean completion). A DEBUG hold
// (issue #39) is reported as a benign failure carrying StateDebug.
func (s *Slot) listenAndRunJob(ctx context.Context, rec *cycle.Record, proc Proc, guest Guest, runnerName string) (bool, State, error) {
	cfg := s.deps.Config
	completed := slices.Clone(rec.States)
	s.setState(StateListening, func(st *Status) {
		st.ActiveCycleStates = completed
		// Reaching LISTENING proves the pre-boot failure (e.g. a doomed image
		// pull) is resolved. Clear the stale NOTE now rather than waiting for
		// finishCycle, so the status reflects "healthy" while the slot is healthy.
		// Both the published fields and the internal counter are reset under the
		// same lock acquisition so observers never see an inconsistent pair.
		st.LastFailure = ""
		st.ConsecutiveFailures = 0
		s.failures = 0
	})
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
				return false, StateListening, fmt.Errorf("%w: %s", errOperatorRecycle, cmd.Reason)
			case CmdPause:
				s.setPaused(true, cmd.ID) // takes effect at next BACKOFF
			case CmdResume:
				s.setPaused(false, cmd.ID)
			case CmdDebugKey:
				// The LISTENING freeze (§5.1).
				done, racedLine, hstate, herr := s.freezeForDebug(ctx, rec, proc, guest, cmd, finishListening)
				switch {
				case racedLine != "":
					// A job started before the request was serviced: fall into
					// the normal JOB transition with the buffered marker; the
					// freeze left the job untouched (decision 15).
					finishListening(cycle.OutcomeOK, "")
					return s.runJob(ctx, rec, proc, guest, racedLine)
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
			s.emitRunnerLine(rec.CycleID, line)
			if !strings.Contains(line, markerJobStarted) {
				continue
			}
			finishListening(cycle.OutcomeOK, "")
			return s.runJob(ctx, rec, proc, guest, line)
		}
	}
}

// runJob drives JOB and, at job end, either tears down or enters a post-job
// DEBUG hold (issue #39). markerLine is the "Running job:" line. ctx is the
// CYCLE context (cctx): the job budget bounds only watchJob's jctx, never the
// operator's mid-job work (§3).
func (s *Slot) runJob(ctx context.Context, rec *cycle.Record, proc Proc, guest Guest, markerLine string) (bool, State, error) {
	cfg := s.deps.Config
	jobName := jobNameFromMarker(markerLine)
	job := &cycle.JobInfo{Name: jobName, Started: time.Now()}
	rec.Job = job
	completedBeforeJob := slices.Clone(rec.States)
	s.setState(StateJob, func(st *Status) {
		st.Job = job
		st.ActiveCycleStates = completedBeforeJob
	})
	jrec := cycle.StateRecord{State: string(StateJob), Entered: time.Now()}

	arm := &debugArm{}
	jctx, jcancel := context.WithTimeout(ctx, cfg.Limits.MaxJobDuration.D())
	jobOK, jobErr := s.watchJob(jctx, ctx, rec, proc, guest, arm)
	jcancel()
	jrec.Left = time.Now()
	if jobOK {
		jrec.Outcome = cycle.OutcomeOK
	} else {
		jrec.Outcome, jrec.Error = cycle.OutcomeError, jobErr.Error()
	}
	rec.States = append(rec.States, jrec)

	switch {
	case !arm.armed:
		// Never armed, or disarmed mid-job: today's returns, verbatim.
	case errors.Is(jobErr, errOperatorRecycle):
		// Cancel consent killed the job: recycle means destroy, hold included.
		s.auditDisarm(rec, "operator recycle canceled the job", slog.LevelInfo)
	case ctx.Err() != nil:
		// Daemon shutdown / cycle cancel: a hold would record a fake DEBUG
		// window.
		s.auditDisarm(rec, "daemon shutdown", slog.LevelInfo)
	case s.isPaused():
		// Decision 19 backstop (a re-arm after pause's disarm, or pause racing
		// the very last loop iteration): pause wins.
		s.auditDisarm(rec, "slot paused", slog.LevelError)
	default:
		holdState, holdErr := s.enterPostJobDebug(ctx, rec, proc, guest, arm)
		if !jobOK {
			// The job's failure owns the cycle (§8): the DEBUG StateRecord is
			// on the record, but the failure is the job's. A failed post-job
			// hold (unproven kill) is recorded in the audit trail; surface it
			// to the daemon log too, since the cycle's failure is the job's.
			if holdErr != nil {
				s.deps.Log.Warn("post-job debug hold failed on a job that also failed; job error owns the cycle, hold failure is in the audit trail", "hold_err", holdErr)
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

func (s *Slot) watchJob(jctx, cctx context.Context, rec *cycle.Record, proc Proc, guest Guest, arm *debugArm) (bool, error) {
	for {
		select {
		case <-jctx.Done():
			// Budget expiry OR daemon shutdown (cctx). Drain-check FIRST
			// (closes the ~20s coin-flip window, §3): a completion marker or
			// channel-close buffered in Lines means the job COMPLETED near the
			// boundary, not a budget blowout.
			if done, ok := s.drainForCompletion(proc, rec.CycleID); done {
				return ok, nil
			}
			return false, fmt.Errorf("job exceeded budget: %w", context.Cause(jctx))

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
			s.emitRunnerLine(rec.CycleID, line)
			if strings.Contains(line, markerJobCompleted) {
				return true, nil
			}

		case cmd := <-s.cmds:
			switch cmd.Kind {
			case CmdPause:
				s.setPaused(true, cmd.ID)
				if arm.armed { // disarm NOW + audit + clear status (decision 19)
					s.auditDisarm(rec, "slot paused", slog.LevelError)
					s.clearArmedStatus(arm, jobDetail(rec))
				}
			case CmdResume:
				s.setPaused(false, cmd.ID)
			case CmdRecycle:
				if cmd.CancelJob {
					if arm.armed { // cancel consent destroys the job AND the armed hold
						s.auditDisarm(rec, "operator recycle canceled the job", slog.LevelInfo)
						s.clearArmedStatus(arm, jobDetail(rec))
					}
					return false, fmt.Errorf("%w: %s (running job canceled)", errOperatorRecycle, cmd.Reason)
				}
				if arm.armed { // plain recycle disarms + audit + clear status (§0)
					s.auditDisarm(rec, "recycled without cancel consent", slog.LevelWarn)
					s.clearArmedStatus(arm, jobDetail(rec))
				}
			case CmdDebugKey:
				s.midJobInject(cctx, rec, guest, arm, cmd)
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

// auditState writes the cycle's InjectedKeys to the operator-access.json
// sidecar (best-effort). Called after every audit mutation so the on-disk
// trail tracks memory; cycle.json itself lands only at finishCycle.
func (s *Slot) writeAuditSidecar(rec *cycle.Record) error {
	data, err := json.MarshalIndent(rec.InjectedKeys, "", "  ")
	if err != nil {
		return err
	}
	store := cycle.Store{SlotDir: s.deps.Home.SlotCyclesDir(s.name)}
	return store.WriteArtifact(rec, cycle.OperatorAccessFile, data)
}

// appendPending appends a write-AHEAD "pending" audit entry and atomically
// writes the sidecar BEFORE any byte reaches the guest. The returned index
// addresses the entry for later updates. On write failure it removes the
// entry and returns ok=false: "no audit, no injection" (decision 4).
func (s *Slot) appendPending(rec *cycle.Record, cmd Command, state State) (int, bool) {
	rec.InjectedKeys = append(rec.InjectedKeys, cycle.InjectedKey{
		Fingerprint: cmd.Fingerprint,
		Comment:     cmd.Comment,
		Injected:    time.Now(),
		Reason:      cmd.Reason,
		Outcome:     "pending",
		State:       string(state),
	})
	idx := len(rec.InjectedKeys) - 1
	if err := s.writeAuditSidecar(rec); err != nil {
		rec.InjectedKeys = rec.InjectedKeys[:idx]
		s.deps.Log.Error("debug: write-ahead audit failed; injection refused", "err", err)
		return 0, false
	}
	return idx, true
}

// updateAudit sets an entry's outcome/error and rewrites the sidecar
// (best-effort; the guest already changed, so a write failure only loses the
// on-disk copy until finishCycle rewrites from memory — decision 4).
func (s *Slot) updateAudit(rec *cycle.Record, idx int, outcome, errStr string) {
	rec.InjectedKeys[idx].Outcome = outcome
	rec.InjectedKeys[idx].Error = errStr
	if err := s.writeAuditSidecar(rec); err != nil {
		s.deps.Log.Error("debug: post-exec audit rewrite failed", "err", err)
	}
}

// auditDisarm appends a "disarmed" entry recording why an armed hold was
// cancelled without DEBUG entry, and rewrites the sidecar (best-effort).
func (s *Slot) auditDisarm(rec *cycle.Record, cause string, level slog.Level) {
	rec.InjectedKeys = append(rec.InjectedKeys, cycle.InjectedKey{
		Injected: time.Now(),
		Outcome:  "disarmed",
		Error:    cause,
		State:    string(StateJob),
	})
	if err := s.writeAuditSidecar(rec); err != nil {
		s.deps.Log.Error("debug: disarm audit rewrite failed", "err", err)
	}
	s.deps.Log.Log(context.Background(), level, "debug hold disarmed", "cause", cause)
}

// clearArmedStatus disarms in memory, clears Status.DebugHoldArmed, and
// rewrites Detail to the plain JOB line — used on every mid-job disarm (plain
// recycle, pause-while-armed). NOT setState (we are in JOB; StateEntered must
// not move). Mirrors the locked helper that SET the flag at arm time.
// recordOperatorKey appends fp to the cycle's job audit (JobInfo.OperatorKeys),
// copy-on-write under the lock. rec.Job and s.status.Job alias one *JobInfo
// (set together at JOB entry), and Status()/the watch feed hand that pointer to
// gRPC goroutines — so an unlocked `append` to its slice is a live data race
// (a torn read of the slice header, or a read of a reallocated backing array).
// Replacing the JobInfo with a fresh copy + fresh slice leaves every snapshot a
// reader already holds immutable, and never mutates a slice another goroutine
// is reading. It then notifies watchers (like clearArmedStatus/setArmedStatus):
// the ambiguous install-failure caller records the possibly-live key and only
// rewrites the cycle sidecar afterward, so without a push here the fact that a
// privileged key may be on the guest stays invisible to StreamStatus until some
// unrelated status change fires.
func (s *Slot) recordOperatorKey(rec *cycle.Record, fp string) {
	s.mu.Lock()
	j := *rec.Job
	j.OperatorKeys = append(append([]string(nil), rec.Job.OperatorKeys...), fp)
	rec.Job = &j
	s.status.Job = &j
	snap := s.status
	fns := slices.Clone(s.onChange)
	s.mu.Unlock()
	s.notify(fns, snap)
}

func (s *Slot) clearArmedStatus(arm *debugArm, detail string) {
	arm.armed = false
	s.mu.Lock()
	s.status.DebugHoldArmed = false
	s.status.Detail = detail
	snap := s.status
	fns := slices.Clone(s.onChange)
	s.mu.Unlock()
	s.notify(fns, snap)
}

// setArmedStatus sets Status.DebugHoldArmed and Detail at arm time (the locked
// mutate-and-notify mirror of clearArmedStatus; NOT setState).
func (s *Slot) setArmedStatus(detail string) {
	s.mu.Lock()
	s.status.DebugHoldArmed = true
	s.status.Detail = detail
	snap := s.status
	fns := slices.Clone(s.onChange)
	s.mu.Unlock()
	s.notify(fns, snap)
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
func (s *Slot) freezeForDebug(ctx context.Context, rec *cycle.Record, proc Proc, guest Guest, cmd Command,
	finishListening func(cycle.Outcome, string),
) (done bool, racedLine string, hstate State, herr error) {
	secureSSH := s.deps.Config.Deadlines.SecureSSH.D()

	// 0. Guards.
	if !cmd.Expires.IsZero() && time.Now().After(cmd.Expires) {
		cmd.reply(DebugKeyReply{Err: errors.New("command expired; nothing was injected")})
		return false, "", "", nil
	}
	if cmd.CycleID != "" && cmd.CycleID != rec.CycleID {
		cmd.reply(DebugKeyReply{Err: fmt.Errorf("cycle %s already ended; nothing was injected", cmd.CycleID)})
		return false, "", "", nil
	}

	// 1. Write-ahead audit.
	idx, ok := s.appendPending(rec, cmd, StateListening)
	if !ok {
		cmd.reply(DebugKeyReply{Err: errors.New("audit write failed; injection refused")})
		return false, "", "", nil
	}

	// 2. Drain-check for a raced job marker / idle runner-exit.
	if line, closed, marker := s.drainListeningLines(proc, rec.CycleID); marker {
		s.updateAudit(rec, idx, "refused", "job started before service")
		cmd.reply(DebugKeyReply{Err: errors.New(
			"a job started before your request was serviced; nothing was injected — re-run debug to inject into the running job",
		)})
		return false, line, "", nil
	} else if closed {
		code, _ := proc.Wait()
		s.updateAudit(rec, idx, "refused", "runner exited while idle")
		cmd.reply(DebugKeyReply{Err: errors.New("the runner exited before the key could be installed; nothing was injected")})
		finishListening(cycle.OutcomeError, fmt.Sprintf("runner exited (code %d) while idle", code))
		return true, "", StateListening, fmt.Errorf("runner exited (code %d) while listening", code)
	}

	// 3. Verified kill — before install, so the operator key never coexists
	// with a live (or ambiguously alive) runner.
	if err := s.boundedGuest(ctx, secureSSH, guest.StopRunner); err != nil {
		s.updateAudit(rec, idx, "error", err.Error())
		cmd.reply(DebugKeyReply{Err: fmt.Errorf("could not verify the runner is dead; nothing was injected: %w", err)})
		finishListening(cycle.OutcomeError, "debug freeze: runner kill unproven")
		return true, "", StateListening, fmt.Errorf("debug freeze kill: %w", errDebugInjectFailed)
	}

	// 4. Drain Lines TO CLOSE (the process is proven dead), bounded 5s. A
	// marker here means a job raced and died with the kill — benign.
	if marker, closed := s.drainToClose(proc, rec.CycleID, 5*time.Second); marker {
		rec.Job = &cycle.JobInfo{Name: "(raced the debug freeze)", Started: time.Now()}
		s.updateAudit(rec, idx, "refused", "job raced the freeze and died with the kill")
		cmd.reply(DebugKeyReply{Err: errors.New("a job started and was killed by the freeze; nothing was injected")})
		finishListening(cycle.OutcomeOK, "")
		return true, "", StateJob, fmt.Errorf("%w", errDebugRacedJob)
	} else if !closed {
		s.updateAudit(rec, idx, "error", "runner output did not close after kill")
		cmd.reply(DebugKeyReply{Err: errors.New("the runner did not close after the kill; nothing was injected")})
		finishListening(cycle.OutcomeError, "debug freeze: runner did not close")
		return true, "", StateListening, fmt.Errorf("debug freeze drain: %w", errDebugInjectFailed)
	}

	// 5. Install.
	if err := s.boundedGuestArg(ctx, secureSSH, cmd.PubKey, guest.InstallAuthorizedKey); err != nil {
		s.updateAudit(rec, idx, "error", err.Error())
		cmd.reply(DebugKeyReply{Err: fmt.Errorf("installing the key failed: %w", err)})
		finishListening(cycle.OutcomeError, "debug freeze: key install failed")
		return true, "", StateListening, fmt.Errorf("debug freeze install: %w", errDebugInjectFailed)
	}

	// 6. Success → DEBUG.
	s.updateAudit(rec, idx, "ok", "")
	holdUntil := time.Now().Add(cmd.Hold)
	cmd.reply(DebugKeyReply{User: s.deps.Pool.SSHUser, HostKeys: guest.HostKeys(), HoldUntil: holdUntil})
	finishListening(cycle.OutcomeOK, "")
	st, err := s.holdForDebug(ctx, rec, guest, holdUntil)
	return true, "", st, err
}

// midJobInject installs an operator key into a RUNNING job's guest WITHOUT
// touching the runner, and arms a post-job DEBUG hold (§5.3). The job is never
// frozen or killed. ctx is the CYCLE context (cctx), not jctx (§3).
func (s *Slot) midJobInject(ctx context.Context, rec *cycle.Record, guest Guest, arm *debugArm, cmd Command) {
	secureSSH := s.deps.Config.Deadlines.SecureSSH.D()
	fp := cmd.Fingerprint

	// 0. Guards.
	if !cmd.Expires.IsZero() && time.Now().After(cmd.Expires) {
		cmd.reply(DebugKeyReply{Err: errors.New("command expired; nothing was injected")})
		return
	}
	if cmd.CycleID != "" && cmd.CycleID != rec.CycleID {
		cmd.reply(DebugKeyReply{Err: fmt.Errorf("cycle %s already ended; nothing was injected", cmd.CycleID)})
		return
	}
	if cmd.SeenState != StateJob {
		// A command aimed at a LISTENING slot that raced into JOB: refuse —
		// converting it into a mid-job injection would write contamination
		// into a CI job's permanent record without consent (decision 15).
		rec.InjectedKeys = append(rec.InjectedKeys, cycle.InjectedKey{
			Fingerprint: fp, Comment: cmd.Comment, Injected: time.Now(), Reason: cmd.Reason,
			Outcome: "refused", State: string(StateJob),
			Error: "operator saw " + string(cmd.SeenState) + ", not JOB",
		})
		_ = s.writeAuditSidecar(rec)
		cmd.reply(DebugKeyReply{Err: errors.New(
			"a job started before your request was serviced; nothing was injected — re-run debug to inject into the running job",
		)})
		return
	}

	// 1. RE-ARM (exec-free) ONLY for a PROVEN-LANDED key.
	if arm.landed(fp) {
		arm.hold = cmd.Hold
		rec.InjectedKeys = append(rec.InjectedKeys, cycle.InjectedKey{
			Fingerprint: fp, Comment: cmd.Comment, Injected: time.Now(), Reason: cmd.Reason,
			Outcome: "re-armed", State: string(StateJob),
		})
		_ = s.writeAuditSidecar(rec)
		s.setArmedStatus(s.armedDetail(fp, arm.hold))
		cmd.reply(s.armedReply(guest))
		return
	}

	// 2. Write-ahead audit.
	idx, ok := s.appendPending(rec, cmd, StateJob)
	if !ok {
		cmd.reply(DebugKeyReply{Err: errors.New("audit write failed; injection refused")})
		return
	}

	// 3. Install over the cycle's live session (a fresh channel; the runner
	// proc and the job are untouched). cctx, NOT jctx (§3).
	err := s.boundedGuestArg(ctx, secureSSH, cmd.PubKey, guest.InstallAuthorizedKey)
	switch {
	case err == nil:
		// 4. Success: arm.
		arm.armed = true
		arm.hold = cmd.Hold
		arm.keys = append(arm.keys, armedKey{fingerprint: fp, landed: true})
		s.recordOperatorKey(rec, fp)
		s.updateAudit(rec, idx, "armed", "")
		s.setArmedStatus(s.armedDetail(fp, arm.hold))
		cmd.reply(s.armedReply(guest))
	case errors.Is(err, ErrGuestUnreachable):
		// NO Redial (decision 18); record not-landed so a retry re-proves.
		arm.keys = append(arm.keys, armedKey{fingerprint: fp, landed: false})
		s.updateAudit(rec, idx, "unreachable", err.Error())
		cmd.reply(DebugKeyReply{Err: fmt.Errorf("guest session unreachable; nothing was injected and the job continues: %w", err)})
	default:
		// Ambiguous: the key may or may not have landed. Record contamination,
		// do NOT arm, the job continues (decision 18).
		arm.keys = append(arm.keys, armedKey{fingerprint: fp, landed: false})
		s.recordOperatorKey(rec, fp)
		s.updateAudit(rec, idx, "error", err.Error())
		cmd.reply(DebugKeyReply{Err: fmt.Errorf("install failed (key state unknown); the job continues: %w", err)})
	}
}

// armedReply is the mid-job armed reply: key live NOW, hold starts at job end
// so HoldUntil is zero (decision 16). The paused-while-arming warning
// (decision 19) is rendered by runnyctl from the slot's paused status, not
// carried here.
func (s *Slot) armedReply(guest Guest) DebugKeyReply {
	return DebugKeyReply{Armed: true, User: s.deps.Pool.SSHUser, HostKeys: guest.HostKeys()}
}

// armedDetail is the JOB+armed status line. hold is the operator's requested
// hold (the one enterPostJobDebug applies at job end), NOT the config cap — the
// live status must report the hold the slot will actually take.
func (s *Slot) armedDetail(fp string, hold time.Duration) string {
	return fmt.Sprintf("debug key installed (%s); holds %s at job end", fp, hold)
}

// enterPostJobDebug runs the post-job freeze tail (§5.4): verified kill, drain
// toward close (force-close tolerated), then the DEBUG hold. The clock starts
// at DEBUG entry (decision 16).
func (s *Slot) enterPostJobDebug(ctx context.Context, rec *cycle.Record, proc Proc, guest Guest, arm *debugArm) (State, error) {
	secureSSH := s.deps.Config.Deadlines.SecureSSH.D()

	// 1. Verified kill, always. At job end the kill prohibition has expired.
	if err := s.boundedGuest(ctx, secureSSH, guest.StopRunner); err != nil {
		rec.InjectedKeys = append(rec.InjectedKeys, cycle.InjectedKey{
			Injected: time.Now(), Outcome: "error", State: string(StateJob),
			Error: "post-job kill unproven: " + err.Error(),
		})
		_ = s.writeAuditSidecar(rec)
		// The hold is NOT entered — an unproven kill must not hold a
		// job-eligible guest. Clear the armed status now so DebugHoldArmed can't
		// linger into teardown.
		s.clearArmedStatus(arm, "post-job kill unproven; tearing down")
		return StateDebug, fmt.Errorf("debug hold entry: %w", errDebugInjectFailed)
	}

	// 2. Drain toward close, bounded 5s, force-close WITHOUT Wait on timeout
	// (the FSM-hang fix, §5.4 step 2).
	if _, closed := s.drainToClose(proc, rec.CycleID, 5*time.Second); !closed {
		proc.Kill() // force-close the client-side channel; do NOT call Wait
		s.deps.Log.Debug("debug: post-job drain did not close; force-closed, exit code unknowable")
	}

	// 3. DEBUG hold; the clock starts NOW.
	holdUntil := time.Now().Add(arm.hold)
	return s.holdForDebug(ctx, rec, guest, holdUntil)
}

// holdForDebug is the frozen DEBUG loop (§5.5); both entry paths share it.
// max-idle is gone by construction; release is destruction.
func (s *Slot) holdForDebug(ctx context.Context, rec *cycle.Record, guest Guest, holdUntil time.Time) (State, error) {
	secureSSH := s.deps.Config.Deadlines.SecureSSH.D()
	completedBeforeDebug := slices.Clone(rec.States)
	s.setState(StateDebug, func(st *Status) {
		st.DebugHoldExpires = holdUntil
		st.Detail = fmt.Sprintf("held for debug; release: runnyctl recycle %s", s.name)
		st.ActiveCycleStates = completedBeforeDebug
	})
	dr := cycle.StateRecord{State: string(StateDebug), Entered: time.Now()}
	finish := func(outcome cycle.Outcome, errStr string) {
		dr.Left, dr.Outcome, dr.Error = time.Now(), outcome, errStr
		rec.States = append(rec.States, dr)
	}

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
		case cmd := <-s.cmds:
			switch cmd.Kind {
			case CmdRecycle:
				finish(cycle.OutcomeOK, "")
				return StateDebug, fmt.Errorf("%w: %s", errOperatorRecycle, cmd.Reason)
			case CmdPause:
				s.setPaused(true, cmd.ID) // holds in the NEXT BACKOFF; the hold itself untouched
			case CmdResume:
				s.setPaused(false, cmd.ID)
			case CmdDebugKey:
				s.debugReArm(ctx, rec, guest, cmd, hold, secureSSH, finish)
				if dr.Left.IsZero() {
					continue // still holding
				}
				return StateDebug, fmt.Errorf("debug hold install: %w", errDebugInjectFailed)
			}
		}
	}
}

// debugReArm handles a CmdDebugKey dequeued in DEBUG (§5.5): re-arm an
// already-installed key exec-free, or install a new key. On a fatal install
// error it calls finish() (setting dr.Left) so the caller ends the hold.
func (s *Slot) debugReArm(ctx context.Context, rec *cycle.Record, guest Guest, cmd Command,
	hold *time.Timer, secureSSH time.Duration, finish func(cycle.Outcome, string),
) {
	if !cmd.Expires.IsZero() && time.Now().After(cmd.Expires) {
		cmd.reply(DebugKeyReply{Err: errors.New("command expired; nothing was injected")})
		return
	}
	if cmd.CycleID != "" && cmd.CycleID != rec.CycleID {
		cmd.reply(DebugKeyReply{Err: fmt.Errorf("cycle %s already ended; nothing was injected", cmd.CycleID)})
		return
	}
	// reset moves the auto-release deadline to now+cmd.Hold and publishes it,
	// returning the new deadline for the reply.
	reset := func() time.Time {
		newUntil := time.Now().Add(cmd.Hold)
		hold.Reset(time.Until(newUntil))
		s.mu.Lock()
		s.status.DebugHoldExpires = newUntil
		snap := s.status
		fns := slices.Clone(s.onChange)
		s.mu.Unlock()
		s.notify(fns, snap)
		return newUntil
	}

	// RE-ARM (exec-free): the fingerprint already has an ok/armed/re-armed
	// entry this cycle (survives a guest reboot).
	if s.keyInstalledThisCycle(rec, cmd.Fingerprint) {
		newUntil := reset()
		rec.InjectedKeys = append(rec.InjectedKeys, cycle.InjectedKey{
			Fingerprint: cmd.Fingerprint, Comment: cmd.Comment, Injected: time.Now(), Reason: cmd.Reason,
			Outcome: "re-armed", State: string(StateDebug),
		})
		_ = s.writeAuditSidecar(rec)
		cmd.reply(DebugKeyReply{User: s.deps.Pool.SSHUser, HostKeys: guest.HostKeys(), HoldUntil: newUntil})
		return
	}

	// NEW KEY: write-ahead, then install with a one-shot Redial retry.
	idx, ok := s.appendPending(rec, cmd, StateDebug)
	if !ok {
		cmd.reply(DebugKeyReply{Err: errors.New("audit write failed; injection refused")})
		return
	}
	err := s.boundedGuestArg(ctx, secureSSH, cmd.PubKey, guest.InstallAuthorizedKey)
	if errors.Is(err, ErrGuestUnreachable) {
		if rerr := s.boundedGuest(ctx, secureSSH, guest.Redial); rerr == nil {
			err = s.boundedGuestArg(ctx, secureSSH, cmd.PubKey, guest.InstallAuthorizedKey)
		}
	}
	switch {
	case err == nil:
		newUntil := reset()
		s.updateAudit(rec, idx, "ok", "")
		cmd.reply(DebugKeyReply{User: s.deps.Pool.SSHUser, HostKeys: guest.HostKeys(), HoldUntil: newUntil})
	case errors.Is(err, ErrGuestUnreachable):
		s.updateAudit(rec, idx, "unreachable", err.Error())
		cmd.reply(DebugKeyReply{Err: errors.New(
			"guest session is down (rebooted?); hold unchanged — extend with the already-installed key, or release with recycle",
		)})
	default:
		s.updateAudit(rec, idx, "error", err.Error())
		cmd.reply(DebugKeyReply{Err: fmt.Errorf("installing the key failed: %w", err)})
		finish(cycle.OutcomeError, "debug hold: key install failed")
	}
}

// keyInstalledThisCycle reports whether fp has an ok/armed/re-armed audit
// entry this cycle — the DEBUG re-arm consults outcomes, not raw membership.
func (s *Slot) keyInstalledThisCycle(rec *cycle.Record, fp string) bool {
	for _, k := range rec.InjectedKeys {
		if k.Fingerprint == fp && (k.Outcome == "ok" || k.Outcome == "armed" || k.Outcome == "re-armed") {
			return true
		}
	}
	return false
}

// drainForCompletion non-blockingly drains proc.Lines after jctx fires: a
// buffered completion marker or a channel-close means the job COMPLETED near
// the budget boundary (§3 coin-flip fix). Returns done=true with the
// completion verdict, or done=false (a genuine blowout).
func (s *Slot) drainForCompletion(proc Proc, cycleID string) (done, ok bool) {
	for {
		select {
		case line, open := <-proc.Lines():
			if !open {
				code, _ := proc.Wait()
				return true, code == 0
			}
			s.emitRunnerLine(cycleID, line)
			if strings.Contains(line, markerJobCompleted) {
				return true, true
			}
		default:
			return false, false
		}
	}
}

// drainListeningLines non-blockingly drains proc.Lines during the LISTENING
// freeze (§5.1 step 2), forwarding each line. Returns the job marker line (if
// any), whether the channel closed, and whether a marker was seen.
func (s *Slot) drainListeningLines(proc Proc, cycleID string) (jobLine string, closed, marker bool) {
	for {
		select {
		case line, open := <-proc.Lines():
			if !open {
				return "", true, false
			}
			s.emitRunnerLine(cycleID, line)
			if strings.Contains(line, markerJobStarted) {
				return line, false, true
			}
		default:
			return "", false, false
		}
	}
}

// drainToClose drains proc.Lines toward close, bounded by d, forwarding lines.
// Returns whether a job marker appeared and whether the channel closed.
func (s *Slot) drainToClose(proc Proc, cycleID string, d time.Duration) (marker, closed bool) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case line, open := <-proc.Lines():
			if !open {
				return marker, true
			}
			s.emitRunnerLine(cycleID, line)
			if strings.Contains(line, markerJobStarted) {
				marker = true
			}
		case <-timer.C:
			return marker, false
		}
	}
}

// boundedGuest calls a guest method that takes only a bounded.Context.
func (s *Slot) boundedGuest(ctx context.Context, d time.Duration, f func(bounded.Context) error) error {
	bctx, cancel := bounded.WithTimeout(ctx, d)
	defer cancel()
	return f(bctx)
}

// boundedGuestArg calls a guest method that takes a bounded.Context and a
// string argument.
func (s *Slot) boundedGuestArg(ctx context.Context, d time.Duration, arg string, f func(bounded.Context, string) error) error {
	bctx, cancel := bounded.WithTimeout(ctx, d)
	defer cancel()
	return f(bctx, arg)
}

// emitRunnerLine forwards one line of guest runner output to the configured
// sink (the runner log ring). The FSM's own reads are the tee points — a
// wrapper goroutine would block (and leak) on lines the FSM stops consuming
// after a marker ends its state; lines nobody reads simply aren't logged,
// matching what an operator could ever have observed.
func (s *Slot) emitRunnerLine(cycleID, line string) {
	if s.deps.OnRunnerLine != nil {
		s.deps.OnRunnerLine(s.name, cycleID, line)
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
	completedBeforeTeardown := slices.Clone(rec.States)
	s.setState(StateTeardown, func(st *Status) { st.ActiveCycleStates = completedBeforeTeardown })
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
		if err := removeAll(in.vmDir); err != nil {
			s.deps.Log.Error("removing vm dir", "err", err)
			cleanupWarns = append(cleanupWarns, fmt.Sprintf("remove clone: %v", err))
		}
	}

	// 5. Deregister iff no job ran (JIT runners self-remove after a job).
	if in.runnerID != 0 && !in.jobRan {
		if err := s.deps.GitHub.DeleteRunner(tctx, in.runnerID); err != nil {
			s.deps.Log.Warn("deregistering runner", "id", in.runnerID, "err", err)
			cleanupWarns = append(cleanupWarns, fmt.Sprintf("deregister runner %d: %v", in.runnerID, err))
		}
	}

	tr.Left = time.Now()
	switch {
	case wedged:
		// Recording OK here once hid the exact outage this project exists
		// to kill: cycle.json swore teardown succeeded while a ghost guest
		// ate the macOS guest cap and every later boot failed. A dereg can
		// still fail on this path (step 5 runs regardless); note it, but the
		// wedge dominates the outcome. tr.Error already holds the stop failure.
		tr.Outcome = cycle.OutcomeError
		if len(cleanupWarns) > 0 {
			tr.Error += "; " + strings.Join(cleanupWarns, "; ")
		}
	case len(cleanupWarns) > 0:
		tr.Outcome = cycle.OutcomeWarn
		tr.Error = strings.Join(cleanupWarns, "; ")
	default:
		tr.Outcome = cycle.OutcomeOK
	}
	rec.States = append(rec.States, tr)
	return wedged
}

// finishCycle writes the record, updates failure accounting, and prunes.
// Benign endings (operator recycle, daemon shutdown) leave the failure
// streak untouched in both directions: they are not health signals, so they
// neither escalate backoff nor launder a failing slot's history (issue #21).
func (s *Slot) finishCycle(rec *cycle.Record, benign bool) {
	store := cycle.Store{SlotDir: s.deps.Home.SlotCyclesDir(s.name)}
	if err := store.Write(rec); err != nil {
		s.deps.Log.Error("writing cycle record", "err", err)
	}
	cfg := s.deps.Config
	if err := store.Prune(cfg.Retention.CyclesPerSlot, cfg.Retention.MaxAge.D(), time.Now()); err != nil {
		s.deps.Log.Warn("pruning cycle records", "err", err)
	}

	s.mu.Lock()
	switch {
	case rec.Result == cycle.ResultSuccess ||
		((heldListening(rec) || debugHeldAfterJobOK(rec)) && !injectionFailed(rec)):
		s.failures = 0
		s.status.LastFailure = ""
	case benign:
		// streak unchanged
	default:
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
// in a non-success (e.g. operator recycle, zombie after hours).
func heldListening(rec *cycle.Record) bool {
	for _, sr := range rec.States {
		if sr.State == string(StateListening) && sr.Left.Sub(sr.Entered) >= 10*time.Minute {
			return true
		}
	}
	return false
}

// debugHeldAfterJobOK resets backoff for a delivered-job-then-benign-hold
// cycle (issue #39, §8): it requires BOTH a JOB StateRecord with Outcome ok
// AND a DEBUG StateRecord. The DEBUG-record requirement keeps a failed DEBUG
// entry (errDebugInjectFailed writes no DEBUG StateRecord) out of the reset
// arm.
func debugHeldAfterJobOK(rec *cycle.Record) bool {
	jobOK, debugSeen := false, false
	for _, sr := range rec.States {
		if sr.State == string(StateJob) && sr.Outcome == cycle.OutcomeOK {
			jobOK = true
		}
		if sr.State == string(StateDebug) {
			debugSeen = true
		}
	}
	return jobOK && debugSeen
}

// injectionFailed: any InjectedKeys entry with Outcome "error" AND State !=
// "JOB" (decision 18: mid-job exec failures are not a health signal; LISTENING
// and DEBUG "error" entries count as before). "refused"/"unreachable"/
// "re-armed"/"disarmed" never count.
func injectionFailed(rec *cycle.Record) bool {
	for _, k := range rec.InjectedKeys {
		if k.Outcome == "error" && k.State != string(StateJob) {
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
