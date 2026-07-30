// Package obs is the structured observability event stream for a runner
// cycle: the one seam every telemetry consumer (OTLP traces, OTLP metrics,
// the per-cycle actions artifact) builds on. The FSM's record helpers emit
// these events at the same instant they append to cycle.Record — one code
// path, two outputs — so telemetry can never disagree with the record.
//
// obs imports nothing beyond the standard library: domain packages call
// Action without importing any telemetry SDK, and the emitter installed by
// the daemon is just a func(Event). Fan-out, buffering, and the
// must-not-block contract belong to whatever installs the emitter, not to
// this package — see the Emitter doc comment.
package obs

import (
	"context"
	"time"
)

// Kind discriminates which payload field on Event is populated.
type Kind string

const (
	KindCycleStarted  Kind = "cycle_started"
	KindStepEntered   Kind = "step_entered"
	KindStepLeft      Kind = "step_left"
	KindActionStarted Kind = "action_started"
	KindActionEnded   Kind = "action_ended"
	// KindHTTP is declared in http.go beside the transport that emits it.
	KindDetail        Kind = "detail"
	KindVMInfo        Kind = "vm_info"
	KindImageInfo     Kind = "image_info"
	KindRunnerInfo    Kind = "runner_info"
	KindJobStarted    Kind = "job_started"
	KindJobEnded      Kind = "job_ended"
	KindAuditAppend   Kind = "audit_append"
	KindAuditUpdate   Kind = "audit_update"
	KindCycleFinished Kind = "cycle_finished"
	// KindTarballDone is cycle-scoped: a runner-tarball download belongs to
	// whichever cycle triggered it, unlike a shared image pull (see pull.go
	// for the pull-scoped kinds).
	KindTarballDone Kind = "tarball_done"
	// KindActionMilestone marks a discrete, named point in time within the
	// currently open action — unlike KindDetail (a last-value-wins
	// progress annotation collapsed onto whichever span closes next),
	// every milestone renders as its own timestamped span event, so a
	// multi-stage action (see Milestone) can show which stage was reached
	// and when, not just its final outcome.
	KindActionMilestone Kind = "action_milestone"
	// KindBackendStarted/KindBackendEnded carry one span from a dependency
	// that instruments itself in-process. runny's traces are assembled here
	// from the event stream rather than from spans held in a caller's
	// context, so such a library has no parent to attach to and would emit
	// its own disconnected roots; adapting it into these events is what puts
	// its work inside the cycle that caused it (see BackendEvent).
	//
	// Paired rather than a single completion event like KindHTTP, because
	// these nest: spans end innermost-first, so a lone end event would
	// describe a child before its parent existed.
	KindBackendStarted Kind = "backend_started"
	KindBackendEnded   Kind = "backend_ended"
)

// Action names are a closed set: each becomes a span name on the trace side
// and an `action` metric label, so an inline string at a call site would
// mint unbounded label cardinality. Add here, never inline.
const (
	// TEARDOWN sub-steps.
	ActionStop             = "stop"               // stop escalation (request-stop → force)
	ActionDeregister       = "deregister"         // GitHub runner delete
	ActionDiagPull         = "diag-pull"          // post-mortem _diag tail fetch
	ActionDebugSessionPull = "debug-session-pull" // operator session recording fetch
	ActionCloneRemove      = "clone-remove"       // vmDir cleanup
	// SECURE_SSH.
	ActionRotate = "rotate" // key mint + install + sshd flip + keyed reconnect
	// PROVISION.
	ActionStartRunner       = "start-runner"        // stage tarball + exec run.sh (one guest exec)
	ActionPushRunnerTarball = "push-runner-tarball" // stream tarball to the guest (no live share device; windows)
	// ENSURE_IMAGE.
	ActionResolve       = "resolve"        // registry manifest round-trip → digest
	ActionTarballEnsure = "tarball-ensure" // runner-tarball resolve + download (or cache hit)
	ActionWaitForPull   = "wait-for-pull"  // time subscribed to the shared pull actor
	// AWAIT_IP (windows only).
	ActionNetworkFixup = "network-fixup" // console-driven netplan fixup fallback
)

// Attr keys are closed-set for the same reason action names are: a typo'd
// inline key silently forks an attribute name.
const (
	// AttrHardening is the SSH hardening mode a rotate action ran under.
	AttrHardening = "runny.hardening"
	// AttrPullID identifies the shared image pull a wait-for-pull action was
	// subscribed to — the correlation handle across the cycles that shared
	// one pull. Action-local by design: identity a cycle LEARNS (digest,
	// runner version) travels as a typed event (ImageEvent), not an attr.
	AttrPullID = "runny.pull.id"
)

// Outcome classifies how a step or action ended. Deliberately a plain
// string, not cycle.Outcome: obs does not import internal/cycle, so the FSM
// is free to record its richer outcome vocabulary (warn, deadline) on the
// cycle record while obs events carry whatever string the emitting call
// site passes.
type Outcome string

const (
	OutcomeOK    Outcome = "ok"
	OutcomeError Outcome = "error"
)

// OutcomeOf maps an error to the ok/error vocabulary — the one definition of
// that mapping, shared by Action and by metric recorders outside this
// package, so the vocabulary can't fork.
func OutcomeOf(err error) Outcome {
	if err != nil {
		return OutcomeError
	}
	return OutcomeOK
}

// ErrText is the one definition of "an event's Error field": empty for a
// nil error, err.Error() otherwise. Shared by Action and by every payload
// that pairs an Outcome with an Error string, so the two can't drift apart.
func ErrText(err error) string {
	if err != nil {
		return err.Error()
	}
	return ""
}

// CycleRef identifies the cycle an event belongs to, plus the cycle-static
// identity consumers attach to everything derived from it (the trace root's
// attributes, the metrics side's pool label): all of it is known at cycle
// start, so it rides the ref instead of needing events of its own.
type CycleRef struct {
	Slot string
	Pool string
	// Image is the pool's configured image ref (intent, from config) — the
	// resolved digest is learned mid-cycle and travels as an ImageEvent.
	Image      string
	CycleID    string
	RunnerName string
	Started    time.Time
}

// StepEvent is the payload for StepEntered/StepLeft. A state is entered at
// most once per cycle — the FSM never revisits a state within a cycle (a
// broken guest means a new cycle, not an in-place retry) — so Event.Step
// (the state the FSM entered before emitting) alone correlates a StepLeft
// with its StepEntered; StepEvent carries no State field of its own.
// Duration is populated on StepLeft only (the FSM knows the step's
// Entered/Left from its own StateRecord); zero on StepEntered.
type StepEvent struct {
	Outcome  Outcome
	Error    string
	Duration time.Duration
}

// Attr is one action attribute. Keys are fully-qualified constants declared
// in this package (see AttrHardening) and consumers pass them through
// verbatim; values must come from small closed sets — never a
// guest-controlled string. The slice a caller passes to Action is retained
// by both emitted events, so it must not be mutated after the call.
type Attr struct {
	Key   string
	Value string
}

// ActionEvent is the payload for ActionStarted/ActionEnded. The step the
// action ran under is the Event's top-level Step, not repeated here. Attrs
// (nil when the action has none) appear identically on both events; check
// with len, never against nil.
type ActionEvent struct {
	Name     string
	Attrs    []Attr
	Outcome  Outcome
	Error    string
	Duration time.Duration
}

// BackendEvent is one span from a self-instrumenting dependency, carried
// across the obs seam so it lands inside the cycle rather than as its own
// root. Both halves of the pair carry ID; only the start half carries Name
// and ParentID, and only the end half carries Attrs, Outcome and Error --
// attributes ride the close because a library sets them on its span after
// starting it, so they do not exist yet when the opening event fires.
//
// ID is unique per span and opaque here — the adapter that produces these
// owns its shape. ParentID empty means "attach to whatever is innermost",
// the same precedence an HTTP round trip gets: the open action, else the
// step, else the cycle root. A non-empty ParentID names an earlier
// BackendEvent's ID and is what preserves the dependency's own nesting.
type BackendEvent struct {
	ID       string
	ParentID string
	Name     string
	Attrs    []Attr
	Outcome  Outcome
	Error    string
}

// DetailEvent carries a live annotation ("2.1 GiB at 41 MiB/s") or, under
// KindActionMilestone, a milestone name (see Milestone) — the shape is the
// same single string either way; Kind decides how a consumer renders it.
type DetailEvent struct {
	Text string
}

// VMEvent carries VM identity learned mid-cycle (MAC, then IP).
type VMEvent struct {
	MAC string
	IP  string
	// The guest's resolved shape as of Boot (see vm.Spec), emitted once
	// alongside MAC.
	GuestOS     string
	Arch        string
	CPUCount    uint
	MemoryBytes uint64
}

// ImageEvent carries image identity learned mid-cycle: the resolved
// manifest digest (as soon as the registry round-trip yields it, before the
// pull), then the runner tarball's asset filename (the record's
// RunnerVersion). Emitted once per learned field, the VMEvent MAC-then-IP
// pattern.
type ImageEvent struct {
	Digest        string
	RunnerVersion string
}

// RunnerEvent carries the GitHub runner registration learned at MINT_JIT.
type RunnerEvent struct {
	ID int64
}

// JobEvent is the payload for JobStarted/JobEnded. OperatorKeys (JobEnded
// only) are the operator debug-key fingerprints present in — or ambiguously
// attempted against — the guest while the job ran, mirroring
// cycle.JobInfo.OperatorKeys: "did this job run with a credential
// installed" answerable from the event stream alone. Duration is populated
// on JobEnded only (the FSM knows the job's Started and its own end time);
// zero on JobStarted.
type JobEvent struct {
	Name         string
	Outcome      Outcome
	OperatorKeys []string
	Duration     time.Duration
}

// AuditEvent mirrors an operator debug-key audit append/update
// (cycle.InjectedKey) — observational only, obs is not the audit's system
// of record. OperatorUID nil with OperatorSID empty means the peer's
// identity could not be read, distinct from a recorded uid 0 (root is a
// real possible peer); a non-numeric identity (a Windows SID) is carried in
// OperatorSID, mirroring the record's own field pair.
type AuditEvent struct {
	Fingerprint  string
	Comment      string
	Reason       string
	Error        string
	Outcome      string
	State        string
	OperatorUID  *uint32
	OperatorSID  string
	OperatorUser string
}

// FinishEvent is the payload for CycleFinished.
type FinishEvent struct {
	Result       string
	Ending       string
	FailureState string
	Error        string
}

// TarballEvent is the payload for KindTarballDone.
type TarballEvent struct {
	Outcome  Outcome
	Error    string
	Duration time.Duration
}

// Event is one entry in a cycle's — or a shared image pull's —
// observability stream. Exactly one kind-specific payload is populated,
// selected by Kind, and exactly one of Cycle/Pull identifies which scope the
// event belongs to (a zero CycleRef when Pull is set). Step is the FSM step
// active when the event was emitted — stamped by Emit from the scope, empty
// for cycle-level events emitted outside any step scope, and always empty
// for pull-scoped events — so every kind carries its step attribution
// without consumers folding StepEntered/StepLeft brackets to recover it.
type Event struct {
	Time  time.Time
	Cycle CycleRef
	Pull  *PullRef
	Step  string
	Kind  Kind

	StepInfo *StepEvent
	Action   *ActionEvent
	Backend  *BackendEvent
	HTTP     *HTTPEvent
	Detail   *DetailEvent
	VM       *VMEvent
	Image    *ImageEvent
	Runner   *RunnerEvent
	Job      *JobEvent
	Audit    *AuditEvent
	Finish   *FinishEvent
	PullInfo *PullEvent
	Tarball  *TarballEvent
}

// Emitter receives every event a scope produces. Installed by the daemon;
// nil means no-op (see WithCycle). It must not block: an installer that
// wires an Emitter to real consumers is responsible for fan-out and for a
// drop-with-logged-counter response to backpressure — the FSM goroutine
// that calls Emit or Action can never be made to wait on a slow consumer.
// The payload types only grow additively over time (e.g. StepEvent.Duration
// and JobEvent.Duration, populated on the *Left/*Ended half of their pair):
// an existing field's meaning or an existing Kind's semantics never change,
// so a consumer written against an older payload shape keeps working.
// All events for one cycle scope are emitted from a single goroutine (the
// slot's FSM goroutine) with two exceptions an emitter must tolerate:
// KindHTTP from a scoped context handed to concurrent requests, and every
// pull-scoped event after the first — KindPullStarted runs on whichever
// cycle's goroutine happens to create the puller (the same goroutine that
// calls WithPull), but everything after it (KindDetail, KindHTTP,
// KindPullFinished) comes from the shared image puller's own goroutine, its
// progress watcher, or its blob-fetch workers — never a cycle's FSM
// goroutine. Consumers correlate and order by each event's own Time and by
// Kind-specific pairing (ActionStarted/ActionEnded matched by name within a
// step, StepEntered/StepLeft by State) — nothing needs a sequence counter.
type Emitter func(Event)

// scope is the context-carried identity: which cycle/step (or pull) is
// active and where events go. pull is nil for a cycle scope; when set, it
// identifies a pull scope and emitEvent stamps Event.Pull instead of
// Event.Cycle. Unexported so it can only be built through
// WithCycle/WithPull/WithStep.
type scope struct {
	emit  Emitter
	cycle CycleRef
	pull  *PullRef
	step  string
}

type scopeKey struct{}

// WithCycle establishes the observability scope for one cycle: every Emit
// and Action on the returned context (or a context derived from it,
// including through bounded.Context, whose Value delegates to its parent)
// emits through emit against cycle. emit may be nil, which makes every Emit
// and Action on this scope a no-op — the shape used when telemetry is
// unconfigured.
func WithCycle(ctx context.Context, emit Emitter, cycle CycleRef) context.Context {
	return context.WithValue(ctx, scopeKey{}, newScope(emit, cycle, nil))
}

// newScope builds the scope both WithCycle and WithPull install, differing
// only in which identity — cycle or pull — is set. pull nil means a cycle
// scope.
func newScope(emit Emitter, cycle CycleRef, pull *PullRef) *scope {
	return &scope{emit: emit, cycle: cycle, pull: pull}
}

// WithStep derives a scope for one FSM step: same cycle, same emitter. On a
// context with no scope it returns ctx unchanged.
func WithStep(ctx context.Context, step string) context.Context {
	s, _ := ctx.Value(scopeKey{}).(*scope)
	if s == nil {
		return ctx
	}
	child := *s
	child.step = step
	return context.WithValue(ctx, scopeKey{}, &child)
}

// liveScope returns ctx's scope when it can actually emit, nil otherwise —
// the one definition of the degradation predicate Emit, Action, and
// HTTPTransport all share, so "no scope or nil emitter means no-op" cannot
// fork between them.
func liveScope(ctx context.Context) *scope {
	s, _ := ctx.Value(scopeKey{}).(*scope)
	if s == nil || s.emit == nil {
		return nil
	}
	return s
}

// Live reports whether ctx carries a scope that can actually emit — the
// same predicate Action and HTTPTransport check internally before doing any
// work. A caller outside this package that builds a payload before calling
// Emit on a hot path should check this first, so a scope-less or
// nil-emitter context never pays for an allocation nobody will read.
func Live(ctx context.Context) bool {
	return liveScope(ctx) != nil
}

// emitEvent stamps e with the current time and the scope's identity —
// CycleRef for a cycle scope, PullRef for a pull scope, exactly one of the
// two — and the scope's step, then hands it to the emitter.
func (s *scope) emitEvent(e Event) {
	e.Time = time.Now()
	if s.pull != nil {
		e.Pull = s.pull
	} else {
		e.Cycle = s.cycle
	}
	e.Step = s.step
	s.emit(e)
}

// Emit stamps e with the current time, the scope's CycleRef, and the
// scope's step, then hands it to the emitter. The caller supplies Kind and
// the kind-specific payload; Time, Cycle, and Step are overwritten. On a
// context with no scope, or a scope with a nil emitter, Emit is a safe
// no-op.
func Emit(ctx context.Context, e Event) {
	if s := liveScope(ctx); s != nil {
		s.emitEvent(e)
	}
}

// Action runs fn, emitting ActionStarted before and ActionEnded after with
// duration, outcome, and fn's error — sugar over Emit. An action name must
// appear at most once per step: consumers pair ActionStarted/ActionEnded by
// (step, name), so a caller that runs the same action twice in one step
// (say, a retried dial) must disambiguate the name itself. attrs (see Attr)
// appear on both events. On a context with no scope (never passed through
// WithCycle) or a scope with a nil emitter, Action degrades to a plain
// fn(ctx) call — zero events, no emitter work. Domain packages call Action
// without knowing or caring which case applies.
func Action(ctx context.Context, name string, fn func(context.Context) error, attrs ...Attr) error {
	if liveScope(ctx) == nil {
		return fn(ctx)
	}

	Emit(ctx, Event{Kind: KindActionStarted, Action: &ActionEvent{Name: name, Attrs: attrs}})

	start := time.Now()
	err := fn(ctx)
	dur := time.Since(start)

	Emit(ctx, Event{Kind: KindActionEnded, Action: &ActionEvent{
		Name:     name,
		Attrs:    attrs,
		Outcome:  OutcomeOf(err),
		Error:    ErrText(err),
		Duration: dur,
	}})

	return err
}

// Milestone marks a discrete, named point in time within the currently open
// action — sugar over Emit(KindActionMilestone), the same way Action is
// sugar over Emit(KindAction{Started,Ended}). Call it only from within (or
// downstream of) an Action's fn: a milestone fired with no action currently
// open on ctx's step has nothing to attach to and is dropped by the trace
// consumer. On a context with no scope (or a nil emitter), a safe no-op —
// domain packages call it without knowing or caring which case applies.
func Milestone(ctx context.Context, name string) {
	Emit(ctx, Event{Kind: KindActionMilestone, Detail: &DetailEvent{Text: name}})
}
