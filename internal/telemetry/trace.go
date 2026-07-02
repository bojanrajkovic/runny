package telemetry

import (
	"context"
	"crypto/rand"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/bojanrajkovic/runny/internal/obs"
	"github.com/bojanrajkovic/runny/internal/traceid"
)

// NewTraceConsumer returns an obs.Emitter that folds one cycle's event
// stream into an OTLP span tree: root "runny.cycle", one child
// "cycle.step <STATE>" per FSM step, one grandchild "cycle.step.action
// <name>" per action within a step. Safe to install as the shared
// obs.Emitter across every slot: assembler state is keyed per (slot, cycle
// ID) behind a package-level mutex on lookup/insert/delete, and each
// cycle's own span handles are additionally guarded by a per-cycle mutex:
// ENSURE_IMAGE's progress callback can fire from the shared image puller's
// own goroutine, not the cycle's FSM goroutine (internal/images/puller.go
// runs each pull on `go p.run()`; internal/statemachine/fsm.go documents
// the resulting KindDetail events as the one case obs's events for a cycle
// aren't all emitted from a single goroutine), so this consumer can't
// assume single-writer either.
func NewTraceConsumer(tracer trace.Tracer) obs.Emitter {
	a := &traceAssembler{tracer: tracer, cycles: map[cycleKey]*cycleSpans{}}
	return a.emit
}

type cycleKey struct{ slot, cycleID string }

type spanHandle struct {
	ctx        context.Context
	span       trace.Span
	lastDetail string
}

type stepSpans struct {
	*spanHandle
	current *spanHandle
	actions map[string]*spanHandle
}

type cycleSpans struct {
	mu      sync.Mutex
	ctx     context.Context
	root    trace.Span
	traceID [16]byte
	steps   map[string]*stepSpans
}

type traceAssembler struct {
	tracer trace.Tracer

	mu     sync.Mutex
	cycles map[cycleKey]*cycleSpans
}

func (a *traceAssembler) get(e obs.Event) *cycleSpans {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cycles[cycleKey{e.Cycle.Slot, e.Cycle.CycleID}]
}

func (a *traceAssembler) emit(e obs.Event) {
	switch e.Kind {
	case obs.KindCycleStarted:
		a.cycleStarted(e)
	case obs.KindStepEntered:
		a.stepEntered(e)
	case obs.KindStepLeft:
		a.stepLeft(e)
	case obs.KindActionStarted:
		a.actionStarted(e)
	case obs.KindActionEnded:
		a.actionEnded(e)
	case obs.KindDetail:
		a.detail(e)
	case obs.KindVMInfo:
		a.vmInfo(e)
	case obs.KindJobStarted:
		a.jobStarted(e)
	case obs.KindAuditAppend, obs.KindAuditUpdate:
		a.audit(e)
	case obs.KindCycleFinished:
		a.cycleFinished(e)
		// KindJobEnded is intentionally dropped: it fires before StepLeft,
		// but its Outcome duplicates the JOB step's own StepLeft outcome,
		// so there's nothing here it would add to the span.
	}
}

func (a *traceAssembler) cycleStarted(e obs.Event) {
	tid := traceid.Trace(e.Cycle.InstancePrefix, e.Cycle.Slot, e.Cycle.CycleID, e.Cycle.Started)
	sid := traceid.Span(tid, "cycle", "", "", e.Seq)
	ctx := withIDs(context.Background(), trace.TraceID(tid), trace.SpanID(sid))
	ctx, span := a.tracer.Start(ctx, "runny.cycle", trace.WithTimestamp(e.Time), trace.WithAttributes(
		attribute.String("runny.slot", e.Cycle.Slot),
		attribute.String("runny.cycle_id", e.Cycle.CycleID),
	))

	a.mu.Lock()
	defer a.mu.Unlock()
	a.cycles[cycleKey{e.Cycle.Slot, e.Cycle.CycleID}] = &cycleSpans{
		ctx: ctx, root: span, traceID: tid, steps: map[string]*stepSpans{},
	}
}

func (a *traceAssembler) stepEntered(e obs.Event) {
	cs := a.get(e)
	if cs == nil {
		return
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()

	sid := traceid.Span(cs.traceID, "step", e.Step, "", e.Seq)
	ctx := withIDs(cs.ctx, trace.TraceID(cs.traceID), trace.SpanID(sid))
	ctx, span := a.tracer.Start(ctx, "cycle.step "+e.Step, trace.WithTimestamp(e.Time))
	cs.steps[e.Step] = &stepSpans{
		spanHandle: &spanHandle{ctx: ctx, span: span},
		actions:    map[string]*spanHandle{},
	}
}

func (a *traceAssembler) stepLeft(e obs.Event) {
	cs := a.get(e)
	if cs == nil {
		return
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()

	ss := cs.steps[e.Step]
	if ss == nil {
		return
	}
	if ss.lastDetail != "" {
		ss.span.SetAttributes(attribute.String("runny.progress.last", ss.lastDetail))
	}
	if e.StepInfo != nil {
		ss.span.SetAttributes(attribute.String("runny.outcome", string(e.StepInfo.Outcome)))
		if outcomeIsFailure(e.StepInfo.Outcome) {
			ss.span.SetStatus(codes.Error, e.StepInfo.Error)
		}
	}
	ss.span.End(trace.WithTimestamp(e.Time))
	delete(cs.steps, e.Step)
}

func (a *traceAssembler) actionStarted(e obs.Event) {
	cs := a.get(e)
	if cs == nil {
		return
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()

	ss := cs.steps[e.Step]
	if ss == nil {
		return
	}
	sid := traceid.Span(cs.traceID, "action", e.Step, e.Action.Name, e.Seq)
	ctx := withIDs(ss.ctx, trace.TraceID(cs.traceID), trace.SpanID(sid))
	ctx, span := a.tracer.Start(ctx, "cycle.step.action "+e.Action.Name, trace.WithTimestamp(e.Time))
	h := &spanHandle{ctx: ctx, span: span}
	ss.actions[e.Action.Name] = h
	ss.current = h
}

func (a *traceAssembler) actionEnded(e obs.Event) {
	cs := a.get(e)
	if cs == nil {
		return
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()

	ss := cs.steps[e.Step]
	if ss == nil {
		return
	}
	h := ss.actions[e.Action.Name]
	if h == nil {
		return
	}
	if h.lastDetail != "" {
		h.span.SetAttributes(attribute.String("runny.progress.last", h.lastDetail))
	}
	if outcomeIsFailure(e.Action.Outcome) {
		h.span.SetStatus(codes.Error, e.Action.Error)
	}
	h.span.End(trace.WithTimestamp(e.Time))
	delete(ss.actions, e.Action.Name)
	if ss.current == h {
		ss.current = nil
	}
}

// detail attaches the latest progress annotation to whichever span is
// innermost when it fires — the open action if one is running, else the
// step, else (no step scope at all) the root — so ENSURE_IMAGE's "2.1 GiB
// at 41 MiB/s" style updates land as one summarizing attribute on the span
// that closes next, instead of one span event per tick.
func (a *traceAssembler) detail(e obs.Event) {
	cs := a.get(e)
	if cs == nil || e.Detail == nil {
		return
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()

	ss := cs.steps[e.Step]
	switch {
	case ss == nil:
		cs.root.SetAttributes(attribute.String("runny.progress.last", e.Detail.Text))
	case ss.current != nil:
		ss.current.lastDetail = e.Detail.Text
	default:
		ss.lastDetail = e.Detail.Text
	}
}

func (a *traceAssembler) vmInfo(e obs.Event) {
	cs := a.get(e)
	if cs == nil || e.VM == nil {
		return
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()

	var attrs []attribute.KeyValue
	if e.VM.MAC != "" {
		attrs = append(attrs, attribute.String("vm.mac", e.VM.MAC))
	}
	if e.VM.IP != "" {
		attrs = append(attrs, attribute.String("vm.ip", e.VM.IP))
	}
	cs.root.SetAttributes(attrs...)
	cs.root.AddEvent("vm_info", trace.WithTimestamp(e.Time), trace.WithAttributes(attrs...))
}

func (a *traceAssembler) jobStarted(e obs.Event) {
	cs := a.get(e)
	if cs == nil || e.Job == nil {
		return
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()

	if ss := cs.steps[e.Step]; ss != nil {
		ss.span.SetAttributes(attribute.String("runny.job.name", e.Job.Name))
	}
}

func (a *traceAssembler) audit(e obs.Event) {
	cs := a.get(e)
	if cs == nil || e.Audit == nil {
		return
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()

	cs.root.AddEvent(string(e.Kind), trace.WithTimestamp(e.Time), trace.WithAttributes(
		attribute.String("runny.audit.fingerprint", e.Audit.Fingerprint),
		attribute.String("runny.audit.outcome", e.Audit.Outcome),
		attribute.String("runny.audit.state", e.Audit.State),
	))
}

// outcomeIsFailure mirrors cycle.OutcomeError/OutcomeDeadline's wire values —
// obs.StepEvent/ActionEvent.Outcome is a plain obs.Outcome (obs does not
// import internal/cycle) carrying whatever string the FSM's runState passed
// through, so a deadline miss ("deadline") must count as a span failure
// alongside obs.OutcomeError, or the exact failure mode bounded.Context
// exists to catch would render as a healthy span. cycle.OutcomeWarn
// ("warn") is deliberately excluded: it means the state's mandatory work
// succeeded and only a best-effort cleanup step failed, so it stays
// visible as the runny.outcome attribute without flipping span status.
func outcomeIsFailure(o obs.Outcome) bool {
	return o == obs.OutcomeError || string(o) == "deadline"
}

// endingIsFailure mirrors cycle.EndingFailure/EndingWedge's wire values —
// obs.FinishEvent.Ending is a plain string (obs does not import
// internal/cycle) so the comparison is against the string itself.
func endingIsFailure(ending string) bool {
	return ending == "failure" || ending == "wedge"
}

func (a *traceAssembler) cycleFinished(e obs.Event) {
	key := cycleKey{e.Cycle.Slot, e.Cycle.CycleID}
	a.mu.Lock()
	cs := a.cycles[key]
	delete(a.cycles, key)
	a.mu.Unlock()
	if cs == nil {
		return
	}

	cs.mu.Lock()
	defer cs.mu.Unlock()

	// A step still open here means teardown never ran its course (a panic,
	// or a bug); close it honestly at the cycle's own end time rather than
	// leaking the span.
	for state, ss := range cs.steps {
		for name, h := range ss.actions {
			h.span.End(trace.WithTimestamp(e.Time))
			delete(ss.actions, name)
		}
		ss.span.End(trace.WithTimestamp(e.Time))
		delete(cs.steps, state)
	}

	if e.Finish != nil {
		cs.root.SetAttributes(
			attribute.String("runny.result", e.Finish.Result),
			attribute.String("runny.ending", e.Finish.Ending),
		)
		if e.Finish.FailureState != "" {
			cs.root.SetAttributes(attribute.String("runny.failure_state", e.Finish.FailureState))
		}
		if endingIsFailure(e.Finish.Ending) {
			cs.root.SetStatus(codes.Error, e.Finish.Error)
		}
	}
	cs.root.End(trace.WithTimestamp(e.Time))
}

// idsKey carries the deterministic (trace ID, span ID) pair idGenerator
// hands back for the span about to start — computed by the assembler from
// internal/traceid, not the generator itself.
type idsKey struct{}

type computedIDs struct {
	trace trace.TraceID
	span  trace.SpanID
}

func withIDs(ctx context.Context, tid trace.TraceID, sid trace.SpanID) context.Context {
	return context.WithValue(ctx, idsKey{}, computedIDs{trace: tid, span: sid})
}

// idGenerator hands back the deterministic IDs the trace assembler
// precomputed and stashed on the context passed to Tracer.Start, falling
// back to random IDs for any span this package didn't start itself —
// nothing else in the process starts spans today, but a generator that
// only works for its own caller is a landmine for whatever changes that
// next.
type idGenerator struct{}

func (idGenerator) NewIDs(ctx context.Context) (trace.TraceID, trace.SpanID) {
	if ids, ok := ctx.Value(idsKey{}).(computedIDs); ok {
		return ids.trace, ids.span
	}
	var tid trace.TraceID
	var sid trace.SpanID
	_, _ = rand.Read(tid[:])
	_, _ = rand.Read(sid[:])
	return tid, sid
}

func (idGenerator) NewSpanID(ctx context.Context, _ trace.TraceID) trace.SpanID {
	if ids, ok := ctx.Value(idsKey{}).(computedIDs); ok {
		return ids.span
	}
	var sid trace.SpanID
	_, _ = rand.Read(sid[:])
	return sid
}

var _ sdktrace.IDGenerator = idGenerator{}
