// Package statemachine is runny's core: one crash-only FSM per runner slot.
// Every state is entered with a context deadline; the only
// response to any failure is TEARDOWN → BACKOFF with capped exponential
// backoff. Teardown cannot fail — escalating force is the floor.
package statemachine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/bojanrajkovic/runny/internal/bounded"
	"github.com/bojanrajkovic/runny/internal/cycle"
	"github.com/bojanrajkovic/runny/internal/github"
	"github.com/bojanrajkovic/runny/internal/home"
	"github.com/bojanrajkovic/runny/internal/obs"
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

// FileCloner CoW-clones a single file (clonefile.Clone's seam): the per-cycle
// runner-tarball clone from the shared store into the slot's own mount.
type FileCloner func(src, dst string) error

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
	// PullDebugSession fetches the operator's session recording at teardown;
	// empty output means the operator never connected — skip the artifact.
	PullDebugSession(ctx bounded.Context) ([]byte, error)
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
	WaitFor(ctx bounded.Context, addr, goos string) (Guest, error)
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
	CloneFile      FileCloner
	RemoveAll      func(string) error
	GitHub         GitHub
	Dial           Dialer
	Log            *slog.Logger
	// OnRunnerLine, when set, receives every line of the guest runner's
	// output (run.sh stdout/stderr) as it arrives — the feed for the
	// runnyctl-visible runner log stream. Called on the FSM's line path; it
	// must not block (sink into a logring.Ring, whose fan-out drops on slow
	// subscribers).
	OnRunnerLine func(slot, cycleID, line string)
	// Events, when set, receives the observability event stream (ADR-0024)
	// for every cycle this slot runs: the same record helpers that build
	// cycle.Record emit these at the same instant, so the two can't
	// disagree. nil = no-op — obs.WithCycle/Emit/Action degrade safely, so
	// every existing test and caller is untouched.
	Events obs.Emitter
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
	// OperatorUID/OperatorUser identify the peer that issued this
	// CmdDebugKey: the kernel-authenticated uid read server-side via
	// SO_PEERCRED, and its username resolved best-effort at the socket layer.
	// nil UID means unknown (non-darwin, or a cred-read failure) — distinct
	// from a recorded uid 0 (root, which bypasses the socket's 0600 mode).
	OperatorUID  *uint32
	OperatorUser string
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
	Slot string
	// Pool is the owning pool's configured name; slot-constant identity,
	// like Slot and Image.
	Pool         string
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

	// cell holds everything the slot's status lock guards (the Status
	// snapshot, the watcher list, paused, the failure streak) — see
	// statuscell.go.
	cell *statusCell
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
		cell: newStatusCell(name, deps.Pool),
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
	s.cell.onChangeAppend(fn)
}

// Name returns the slot's immutable name (the status snapshot's Slot field
// is empty until the first state transition; lookups must not depend on it).
func (s *Slot) Name() string { return s.name }

// Status returns the current snapshot.
func (s *Slot) Status() Status {
	return s.cell.snapshot()
}

func (s *Slot) setState(state State, mut func(*Status)) {
	setStatus(s.cell, s.deps.Log, s.name, s.deps.Pool, state, mut)
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
	snap, fns := s.cell.update(func(st *Status) {
		st.Wedged = true
		st.Detail = "guest survived force-stop; slot parked until the daemon restarts"
	})
	s.deps.Log.Error("slot wedged: guest survived force-stop and holds a guest-cap slot; parking (the daemon restarts cold once no job is running)")
	s.cell.notify(fns, snap)
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

func (s *Slot) isPaused() bool {
	return s.cell.isPaused()
}

// setPaused applies a pause/resume and republishes the slot status; see
// statusCell.setPaused.
func (s *Slot) setPaused(p bool, cmdID string) {
	s.cell.setPaused(p, cmdID)
}

func (s *Slot) currentBackoff() time.Duration {
	n := s.cell.failureCount()
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

// runCycle builds this cycle's run and delegates to it; see run.runCycle for
// the state sequence (ENSURE_IMAGE..TEARDOWN).
func (s *Slot) runCycle(ctx context.Context) (*cycle.Record, bool, bool) {
	rec := &cycle.Record{
		CycleID: cycle.NewID(),
		Slot:    s.name,
		Image:   s.deps.Pool.Image, // intent recorded at cycle start, before any state runs
		Started: time.Now(),
	}
	c := &run{
		cell:       s.cell,
		deps:       s.deps,
		cmds:       s.cmds,
		name:       s.name,
		store:      cycle.Store{SlotDir: s.deps.Home.SlotCyclesDir(s.name)},
		rec:        rec,
		runnerName: fmt.Sprintf("%s-%s-%s", s.deps.InstancePrefix, s.name, rec.CycleID),
	}
	return c.runCycle(ctx)
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

	// A raw cell lock, not update(): this joint failures+status write has no
	// notify (finishCycle's caller starts the next backoffWait, whose own
	// setState broadcasts the result), and failures isn't a Status field
	// update() could reach.
	s.cell.mu.Lock()
	switch {
	case rec.Result == cycle.ResultSuccess ||
		((heldListening(rec) || debugHeldAfterJobOK(rec)) && !injectionFailed(rec)):
		s.cell.failures = 0
		s.cell.status.LastFailure = ""
	case benign:
		// streak unchanged
	default:
		s.cell.failures++
		if rec.Failure != nil {
			s.cell.status.LastFailure = rec.Failure.State + ": " + rec.Failure.Error
		}
	}
	s.cell.status.ConsecutiveFailures = s.cell.failures
	s.cell.mu.Unlock()
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
