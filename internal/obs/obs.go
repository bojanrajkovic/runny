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
	KindDetail        Kind = "detail"
	KindVMInfo        Kind = "vm_info"
	KindJobStarted    Kind = "job_started"
	KindJobEnded      Kind = "job_ended"
	KindAuditAppend   Kind = "audit_append"
	KindAuditUpdate   Kind = "audit_update"
	KindCycleFinished Kind = "cycle_finished"
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

// CycleRef identifies the cycle an event belongs to.
type CycleRef struct {
	InstancePrefix string
	Slot           string
	CycleID        string
	Started        time.Time
}

// StepEvent is the payload for StepEntered/StepLeft.
type StepEvent struct {
	State   string
	Outcome Outcome
	Error   string
}

// ActionEvent is the payload for ActionStarted/ActionEnded.
type ActionEvent struct {
	Step     string
	Name     string
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

// JobEvent is the payload for JobStarted/JobEnded.
type JobEvent struct {
	Name    string
	Outcome Outcome
}

// AuditEvent mirrors an operator debug-key audit append/update
// (cycle.InjectedKey) — observational only, obs is not the audit's system
// of record.
type AuditEvent struct {
	Fingerprint string
	Outcome     string
	State       string
}

// FinishEvent is the payload for CycleFinished.
type FinishEvent struct {
	Result       string
	Ending       string
	FailureState string
	Error        string
}

// Event is one entry in a cycle's observability stream. Exactly one
// kind-specific payload is populated, selected by Kind.
type Event struct {
	Seq   uint64
	Time  time.Time
	Cycle CycleRef
	Kind  Kind

	Step   *StepEvent
	Action *ActionEvent
	Detail *DetailEvent
	VM     *VMEvent
	Job    *JobEvent
	Audit  *AuditEvent
	Finish *FinishEvent
}

// Emitter receives every event a scope produces. Installed by the daemon;
// nil means no-op (see WithScope). Emit must not block: an installer that
// wires an Emitter to real consumers is responsible for fan-out and for a
// drop-with-logged-counter response to backpressure — the FSM goroutine
// that calls Action can never be made to wait on a slow consumer.
type Emitter func(Event)

// scope is the context-carried identity: which cycle/step is active, where
// events go, and the per-cycle Seq counter. Unexported so it can only be
// built through WithScope.
type scope struct {
	emit  Emitter
	cycle CycleRef
	step  string
	seq   *atomic.Uint64
}

type scopeKey struct{}

// WithScope attaches observability identity to ctx: every Action called on
// the returned context (or a context derived from it, including through
// bounded.Context, whose Value delegates to its parent) emits through emit
// against cycle/step. emit may be nil, which makes every Action on this
// scope a no-op passthrough — the shape used when telemetry is
// unconfigured.
func WithScope(ctx context.Context, emit Emitter, cycle CycleRef, step string) context.Context {
	return context.WithValue(ctx, scopeKey{}, &scope{
		emit:  emit,
		cycle: cycle,
		step:  step,
		seq:   new(atomic.Uint64),
	})
}

func (s *scope) nextSeq() uint64 {
	return s.seq.Add(1)
}

func (s *scope) emitEvent(kind Kind, action *ActionEvent) {
	if s.emit == nil {
		return
	}
	s.emit(Event{
		Seq:    s.nextSeq(),
		Time:   time.Now(),
		Cycle:  s.cycle,
		Kind:   kind,
		Action: action,
	})
}

// Action runs fn, emitting ActionStarted before and ActionEnded after with
// duration, outcome, and fn's error. On a context with no scope (never
// passed through WithScope) or a scope with a nil emitter, Action degrades
// to a plain fn(ctx) call — zero events, no allocation beyond what fn does.
// Domain packages call Action without knowing or caring which case applies.
func Action(ctx context.Context, name string, fn func(context.Context) error) error {
	s, _ := ctx.Value(scopeKey{}).(*scope)
	if s == nil || s.emit == nil {
		return fn(ctx)
	}

	s.emitEvent(KindActionStarted, &ActionEvent{Step: s.step, Name: name})

	start := time.Now()
	err := fn(ctx)
	dur := time.Since(start)

	outcome, errText := OutcomeOK, ""
	if err != nil {
		outcome, errText = OutcomeError, err.Error()
	}
	s.emitEvent(KindActionEnded, &ActionEvent{
		Step:     s.step,
		Name:     name,
		Outcome:  outcome,
		Error:    errText,
		Duration: dur,
	})

	return err
}
