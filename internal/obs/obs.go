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
	"sync/atomic"
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
	ActionStartRunner = "start-runner" // stage tarball + exec run.sh (one guest exec)
	// ENSURE_IMAGE.
	ActionResolve       = "resolve"        // registry manifest round-trip → digest
	ActionTarballEnsure = "tarball-ensure" // runner-tarball resolve + download (or cache hit)
	ActionWaitForPull   = "wait-for-pull"  // time subscribed to the shared pull actor
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

// CycleRef identifies the cycle an event belongs to, plus the cycle-static
// identity consumers attach to everything derived from it (the trace root's
// attributes, the metrics side's pool label): all of it is known at cycle
// start, so it rides the ref instead of needing events of its own.
type CycleRef struct {
	InstancePrefix string
	Slot           string
	Pool           string
	// Image is the pool's configured image ref (intent, from config) — the
	// resolved digest is learned mid-cycle and travels as an ImageEvent.
	Image      string
	CycleID    string
	RunnerName string
	Started    time.Time
}

// StepEvent is the payload for StepEntered/StepLeft. A state is entered at
// most once per cycle — the FSM never revisits a state within a cycle (a
// broken guest means a new cycle, not an in-place retry) — so State alone
// correlates a StepLeft with its StepEntered.
type StepEvent struct {
	State   string
	Outcome Outcome
	Error   string
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

// DetailEvent carries a live annotation ("2.1 GiB at 41 MiB/s").
type DetailEvent struct {
	Text string
}

// VMEvent carries VM identity learned mid-cycle (MAC, then IP).
type VMEvent struct {
	MAC string
	IP  string
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
// installed" answerable from the event stream alone.
type JobEvent struct {
	Name         string
	Outcome      Outcome
	OperatorKeys []string
}

// AuditEvent mirrors an operator debug-key audit append/update
// (cycle.InjectedKey) — observational only, obs is not the audit's system
// of record. OperatorUID is nil when the peer's uid could not be read,
// distinct from a recorded uid 0 (root is a real possible peer).
type AuditEvent struct {
	Fingerprint  string
	Comment      string
	Reason       string
	Error        string
	Outcome      string
	State        string
	OperatorUID  *uint32
	OperatorUser string
}

// FinishEvent is the payload for CycleFinished.
type FinishEvent struct {
	Result       string
	Ending       string
	FailureState string
	Error        string
}

// Event is one entry in a cycle's observability stream. Exactly one
// kind-specific payload is populated, selected by Kind. Step is the FSM
// step active when the event was emitted — stamped by Emit from the scope,
// empty for cycle-level events emitted outside any step scope — so every
// kind carries its step attribution without consumers folding
// StepEntered/StepLeft brackets to recover it.
type Event struct {
	Seq   uint64
	Time  time.Time
	Cycle CycleRef
	Step  string
	Kind  Kind

	StepInfo *StepEvent
	Action   *ActionEvent
	HTTP     *HTTPEvent
	Detail   *DetailEvent
	VM       *VMEvent
	Image    *ImageEvent
	Runner   *RunnerEvent
	Job      *JobEvent
	Audit    *AuditEvent
	Finish   *FinishEvent
}

// Emitter receives every event a scope produces. Installed by the daemon;
// nil means no-op (see WithCycle). It must not block: an installer that
// wires an Emitter to real consumers is responsible for fan-out and for a
// drop-with-logged-counter response to backpressure — the FSM goroutine
// that calls Emit or Action can never be made to wait on a slow consumer.
// All events for one cycle scope are emitted from a single goroutine (the
// slot's FSM goroutine) with two exceptions an emitter must tolerate: the
// shared image puller's KindDetail progress (see internal/statemachine),
// and KindHTTP from a scoped context handed to concurrent requests (the
// scope's Seq counter is atomic, so order stays coherent). Seq is the
// durable per-cycle order consumers sort and correlate by, not a
// concurrency serializer.
type Emitter func(Event)

// scope is the context-carried identity: which cycle/step is active, where
// events go, and the per-cycle Seq counter. The counter belongs to the
// cycle scope established by WithCycle; step scopes derived by WithStep
// share it — that sharing is what keeps Seq totally ordered across a whole
// cycle. Unexported so it can only be built through WithCycle/WithStep.
type scope struct {
	emit  Emitter
	cycle CycleRef
	step  string
	seq   *atomic.Uint64
}

type scopeKey struct{}

// WithCycle establishes the observability scope for one cycle: every Emit
// and Action on the returned context (or a context derived from it,
// including through bounded.Context, whose Value delegates to its parent)
// emits through emit against cycle, stamped from the single per-cycle Seq
// counter this call creates. emit may be nil, which makes every Emit and
// Action on this scope a no-op — the shape used when telemetry is
// unconfigured.
func WithCycle(ctx context.Context, emit Emitter, cycle CycleRef) context.Context {
	return context.WithValue(ctx, scopeKey{}, &scope{
		emit:  emit,
		cycle: cycle,
		seq:   new(atomic.Uint64),
	})
}

// WithStep derives a scope for one FSM step: same cycle, same emitter, and
// crucially the same per-cycle Seq counter — entering a new step never
// resets the cycle's event order. On a context with no scope it returns
// ctx unchanged.
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

// emitEvent stamps e with the scope's next Seq, the current time, the
// scope's CycleRef, and the scope's step, then hands it to the emitter.
func (s *scope) emitEvent(e Event) {
	e.Seq = s.seq.Add(1)
	e.Time = time.Now()
	e.Cycle = s.cycle
	e.Step = s.step
	s.emit(e)
}

// Emit stamps e with the scope's next Seq, the current time, the scope's
// CycleRef, and the scope's step, then hands it to the emitter. The caller
// supplies Kind and the kind-specific payload; Seq, Time, Cycle, and Step
// are overwritten. On a context with no scope, or a scope with a nil
// emitter, Emit is a safe no-op.
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

	outcome, errText := OutcomeOf(err), ""
	if err != nil {
		errText = err.Error()
	}
	Emit(ctx, Event{Kind: KindActionEnded, Action: &ActionEvent{
		Name:     name,
		Attrs:    attrs,
		Outcome:  outcome,
		Error:    errText,
		Duration: dur,
	}})

	return err
}
