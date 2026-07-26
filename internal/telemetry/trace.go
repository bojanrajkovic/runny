package telemetry

import (
	"context"
	"strconv"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/bojanrajkovic/runny/internal/obs"
)

// NewTraceConsumer returns an obs.Emitter that folds two independent event
// streams into OTLP span trees. A cycle's stream renders as root
// "runny.cycle", one child "cycle.step <STATE>" per FSM step, one
// grandchild "cycle.step.action <name>" per action within a step. A shared
// image pull's stream — scoped separately (obs.WithPull), since a pull
// belongs to no single cycle — renders as its own flat root "runny.pull"
// with "http <class>" client children; the two subtrees correlate by the
// `runny.pull.id` attribute they share (the pull root carries it, and so
// does the cycle-scoped `wait-for-pull` action that waited on it), not by
// span parentage. Safe to install as the shared obs.Emitter across every
// slot: assembler state is keyed per (slot, cycle ID) or per pull ID behind
// a package-level mutex on lookup/insert/delete, and each cycle's or pull's
// own span handles are additionally guarded by a per-entry mutex — a pull's
// events (and ENSURE_IMAGE's KindDetail progress relay) can fire from the
// shared image puller's own goroutine, never a cycle's FSM goroutine, so
// this consumer can't assume single-writer either.
func NewTraceConsumer(tracer trace.Tracer) obs.Emitter {
	a := &traceAssembler{tracer: tracer, cycles: map[cycleKey]*cycleSpans{}, pulls: map[string]*pullSpans{}}
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
	mu    sync.Mutex
	ctx   context.Context
	root  trace.Span
	steps map[string]*stepSpans
}

// pullSpans is a shared image pull's span state — the pull-scope sibling of
// cycleSpans. A pull has no steps or actions of its own (KindHTTP is its
// only child), so it needs none of cycleSpans' step-tracking machinery.
// ref identifies which puller instance this entry belongs to: PullRef.ID is
// a deterministic hash of the pull's directory, so a successor puller for
// the same image (started the instant the last subscriber leaves, before
// the predecessor's own goroutine notices cancellation) reuses the exact
// same map key. Every event from one puller instance carries the identical
// *obs.PullRef pointer (stamped once at obs.WithPull, never copied), so
// comparing ref by pointer identity — not just the map key — is what tells
// a stale predecessor's terminal event from the live successor sharing its
// key: see getPull/resolveOwnPull.
type pullSpans struct {
	mu         sync.Mutex
	ctx        context.Context
	root       trace.Span
	lastDetail string
	ref        *obs.PullRef
}

type traceAssembler struct {
	tracer trace.Tracer

	mu     sync.Mutex
	cycles map[cycleKey]*cycleSpans
	pulls  map[string]*pullSpans
}

// withCycle looks up e's cycle and runs fn with it locked; a no-op if the
// cycle isn't tracked (already finished, or a stray event before it
// started).
func (a *traceAssembler) withCycle(e obs.Event, fn func(cs *cycleSpans)) {
	a.mu.Lock()
	cs := a.cycles[cycleKey{e.Cycle.Slot, e.Cycle.CycleID}]
	a.mu.Unlock()
	if cs == nil {
		return
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	fn(cs)
}

// getPull looks up e.Pull.ID's entry, but only returns it when the entry
// still belongs to the puller instance that emitted e (ps.ref == e.Pull, a
// pointer comparison) — a stale predecessor's event arriving after a
// successor has already overwritten the map entry (see pullSpans' doc
// comment) must not resolve to the successor's live span.
func (a *traceAssembler) getPull(e obs.Event) *pullSpans {
	a.mu.Lock()
	defer a.mu.Unlock()
	ps := a.pulls[e.Pull.ID]
	if ps == nil || ps.ref != e.Pull {
		return nil
	}
	return ps
}

// resolveOwnPull is getPull plus eviction: it deletes the map entry too, but
// ONLY when the looked-up entry is confirmed to belong to e's puller
// instance — the same ref check as getPull. A stale predecessor's terminal
// event must never evict a live successor's entry.
func (a *traceAssembler) resolveOwnPull(e obs.Event) *pullSpans {
	a.mu.Lock()
	defer a.mu.Unlock()
	ps := a.pulls[e.Pull.ID]
	if ps == nil || ps.ref != e.Pull {
		return nil
	}
	delete(a.pulls, e.Pull.ID)
	return ps
}

// withPull looks up e's pull and runs fn with it locked; a no-op if the pull
// isn't tracked (already finished, or a stray event before KindPullStarted).
func (a *traceAssembler) withPull(e obs.Event, fn func(ps *pullSpans)) {
	ps := a.getPull(e)
	if ps == nil {
		return
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	fn(ps)
}

// withStep is withCycle plus an e.Step lookup; a no-op if the step isn't
// open (already left, or a stray event before it started).
func (a *traceAssembler) withStep(e obs.Event, fn func(cs *cycleSpans, ss *stepSpans)) {
	a.withCycle(e, func(cs *cycleSpans) {
		ss := cs.steps[e.Step]
		if ss == nil {
			return
		}
		fn(cs, ss)
	})
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
	case obs.KindHTTP:
		a.httpRoundTrip(e)
	case obs.KindDetail:
		a.detail(e)
	case obs.KindActionMilestone:
		a.actionMilestone(e)
	case obs.KindVMInfo:
		a.vmInfo(e)
	case obs.KindImageInfo:
		a.imageInfo(e)
	case obs.KindRunnerInfo:
		a.runnerInfo(e)
	case obs.KindJobStarted:
		a.jobStarted(e)
	case obs.KindJobEnded:
		a.jobEnded(e)
	case obs.KindAuditAppend, obs.KindAuditUpdate:
		a.audit(e)
	case obs.KindCycleFinished:
		a.cycleFinished(e)
	case obs.KindPullStarted:
		a.pullStarted(e)
	case obs.KindPullFinished:
		a.pullFinished(e)
	case obs.KindPullAbandoned:
		a.pullAbandoned(e)
	}
}

func (a *traceAssembler) cycleStarted(e obs.Event) {
	ctx, span := a.tracer.Start(context.Background(), "runny.cycle", trace.WithTimestamp(e.Time), trace.WithAttributes(
		attribute.String("runny.slot", e.Cycle.Slot),
		attribute.String("runny.pool", e.Cycle.Pool),
		attribute.String("runny.image.ref", e.Cycle.Image),
		attribute.String("runny.cycle_id", e.Cycle.CycleID),
		attribute.String("runny.runner_name", e.Cycle.RunnerName),
	))

	a.mu.Lock()
	defer a.mu.Unlock()
	a.cycles[cycleKey{e.Cycle.Slot, e.Cycle.CycleID}] = &cycleSpans{
		ctx: ctx, root: span, steps: map[string]*stepSpans{},
	}
}

func (a *traceAssembler) stepEntered(e obs.Event) {
	a.withCycle(e, func(cs *cycleSpans) {
		ctx, span := a.tracer.Start(cs.ctx, "cycle.step "+e.Step, trace.WithTimestamp(e.Time))
		cs.steps[e.Step] = &stepSpans{
			spanHandle: &spanHandle{ctx: ctx, span: span},
			actions:    map[string]*spanHandle{},
		}
	})
}

func (a *traceAssembler) stepLeft(e obs.Event) {
	a.withStep(e, func(cs *cycleSpans, ss *stepSpans) {
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
	})
}

func (a *traceAssembler) actionStarted(e obs.Event) {
	a.withStep(e, func(cs *cycleSpans, ss *stepSpans) {
		attrs := make([]attribute.KeyValue, 0, len(e.Action.Attrs))
		for _, kv := range e.Action.Attrs {
			attrs = append(attrs, attribute.String(kv.Key, kv.Value))
		}
		// The action's context is kept so KindHTTP round trips that fire
		// while it's open can parent under it.
		ctx, span := a.tracer.Start(ss.ctx, "cycle.step.action "+e.Action.Name,
			trace.WithTimestamp(e.Time), trace.WithAttributes(attrs...))
		h := &spanHandle{ctx: ctx, span: span}
		ss.actions[e.Action.Name] = h
		ss.current = h
	})
}

func (a *traceAssembler) actionEnded(e obs.Event) {
	a.withStep(e, func(cs *cycleSpans, ss *stepSpans) {
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
	})
}

// httpRoundTrip renders one KindHTTP event as an already-completed child
// span of whatever was innermost when the round trip finished: for a
// cycle-scoped event, the open action if one is running, else the step,
// else the root; for a pull-scoped event (the shared image puller's own
// OCI traffic), always the pull's root — a pull has no steps or actions of
// its own. The event is emitted at completion carrying its own duration, so
// the span starts at Time − Duration and ends at Time — no pairing state to
// hold. Span IDs are SDK-random (round trips of one class legitimately
// repeat within a scope — retries, the registry's 401 token dance — so
// nothing about the event itself need be a unique identity); correlation
// across a trace or across traces is attribute-based (runny.cycle_id on the
// cycle root, runny.pull.id on both the pull root and the cycle-scoped
// wait-for-pull action that waited on it).
func (a *traceAssembler) httpRoundTrip(e obs.Event) {
	if e.HTTP == nil {
		return
	}
	if e.Pull != nil {
		a.withPull(e, func(ps *pullSpans) { a.renderHTTPSpan(ps.ctx, e) })
		return
	}
	a.withCycle(e, func(cs *cycleSpans) {
		parent := cs.ctx
		if ss := cs.steps[e.Step]; ss != nil {
			parent = ss.ctx
			if ss.current != nil {
				parent = ss.current.ctx
			}
		}
		a.renderHTTPSpan(parent, e)
	})
}

// renderHTTPSpan builds the completed client span for one KindHTTP event
// under parent — the part httpRoundTrip's cycle- and pull-scoped routing
// share; only parent selection differs between them.
func (a *traceAssembler) renderHTTPSpan(parent context.Context, e obs.Event) {
	h := e.HTTP
	attrs := []attribute.KeyValue{
		attribute.String("http.request.method", h.Method),
		attribute.String("server.address", h.Host),
	}
	if h.Status != 0 {
		attrs = append(attrs, attribute.Int("http.response.status_code", h.Status))
		attrs = append(attrs, attribute.Int64("runny.http.bytes", h.BytesRead))
	}
	start := e.Time.Add(-h.Duration)
	_, span := a.tracer.Start(parent, "http "+string(h.Class),
		trace.WithTimestamp(start),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attrs...))
	// The span covers the whole exchange through body completion; the
	// headers marker shows where waiting ended and transfer began.
	if h.HeaderDuration > 0 {
		span.AddEvent("headers", trace.WithTimestamp(start.Add(h.HeaderDuration)))
	}
	// Client-span status follows the HTTP semconv rule: any 4xx/5xx is an
	// error, not just transport failures — a 503 retry storm or a 403
	// mint must not render as a row of healthy spans under a red action.
	// (The registry's routine 401 token challenge renders as an errored
	// hop too; its resolve action staying green is what says the dance
	// succeeded.)
	switch {
	case h.Error != "":
		span.SetStatus(codes.Error, h.Error)
	case h.Status >= 400:
		span.SetStatus(codes.Error, "HTTP "+strconv.Itoa(h.Status))
	}
	span.End(trace.WithTimestamp(e.Time))
}

// detail attaches the latest progress annotation to whichever span is
// innermost when it fires. For a cycle-scoped event: the open action if one
// is running, else the step, else (no step scope at all) the root — so
// ENSURE_IMAGE's "2.1 GiB at 41 MiB/s" style updates land as one summarizing
// attribute on the span that closes next, instead of one span event per
// tick. For a pull-scoped event: the pull's root, the only span it has.
func (a *traceAssembler) detail(e obs.Event) {
	if e.Detail == nil {
		return
	}
	if e.Pull != nil {
		a.withPull(e, func(ps *pullSpans) { ps.lastDetail = e.Detail.Text })
		return
	}
	a.withCycle(e, func(cs *cycleSpans) {
		ss := cs.steps[e.Step]
		switch {
		case ss == nil:
			cs.root.SetAttributes(attribute.String("runny.progress.last", e.Detail.Text))
		case ss.current != nil:
			ss.current.lastDetail = e.Detail.Text
		default:
			ss.lastDetail = e.Detail.Text
		}
	})
}

// actionMilestone renders one KindActionMilestone as a genuine, distinctly-
// timestamped span event on the currently open action — unlike detail's
// last-value-wins attribute, every milestone survives independently, so a
// multi-stage action's trace shows which stage was reached and when, not
// just its final outcome. Action-scoped only, by design: a milestone fired
// with no action currently open (misuse — see Milestone's own doc comment)
// has nothing to attach to and is silently dropped, the same degrade-to-
// no-op the rest of this package uses for a stray or scope-less event.
func (a *traceAssembler) actionMilestone(e obs.Event) {
	if e.Detail == nil {
		return
	}
	a.withStep(e, func(cs *cycleSpans, ss *stepSpans) {
		if ss.current == nil {
			return
		}
		ss.current.span.AddEvent(e.Detail.Text, trace.WithTimestamp(e.Time))
	})
}

func (a *traceAssembler) vmInfo(e obs.Event) {
	if e.VM == nil {
		return
	}
	a.withCycle(e, func(cs *cycleSpans) {
		var attrs []attribute.KeyValue
		if e.VM.MAC != "" {
			attrs = append(attrs, attribute.String("vm.mac", e.VM.MAC))
		}
		if e.VM.IP != "" {
			attrs = append(attrs, attribute.String("vm.ip", e.VM.IP))
		}
		if e.VM.GuestOS != "" {
			attrs = append(attrs, attribute.String("vm.guest_os", e.VM.GuestOS))
		}
		if e.VM.Arch != "" {
			attrs = append(attrs, attribute.String("vm.arch", e.VM.Arch))
		}
		if e.VM.CPUCount != 0 {
			attrs = append(attrs, attribute.Int("vm.cpu_count", int(e.VM.CPUCount)))
		}
		if e.VM.MemoryBytes != 0 {
			attrs = append(attrs, attribute.Int64("vm.memory_bytes", int64(e.VM.MemoryBytes)))
		}
		cs.root.SetAttributes(attrs...)
		cs.root.AddEvent("vm_info", trace.WithTimestamp(e.Time), trace.WithAttributes(attrs...))
	})
}

// imageInfo lifts mid-cycle image identity onto the cycle root AND the
// owning step span (ENSURE_IMAGE, where it's learned): the root makes traces
// queryable by image without joining cycle.json, the step keeps the identity
// next to the resolve/wait-for-pull actions that produced it.
func (a *traceAssembler) imageInfo(e obs.Event) {
	if e.Image == nil {
		return
	}
	var attrs []attribute.KeyValue
	if e.Image.Digest != "" {
		attrs = append(attrs, attribute.String("runny.image.digest", e.Image.Digest))
	}
	if e.Image.RunnerVersion != "" {
		attrs = append(attrs, attribute.String("runny.runner_version", e.Image.RunnerVersion))
	}
	a.withCycle(e, func(cs *cycleSpans) {
		cs.root.SetAttributes(attrs...)
		if ss := cs.steps[e.Step]; ss != nil {
			ss.span.SetAttributes(attrs...)
		}
	})
}

func (a *traceAssembler) runnerInfo(e obs.Event) {
	if e.Runner == nil {
		return
	}
	a.withStep(e, func(cs *cycleSpans, ss *stepSpans) {
		ss.span.SetAttributes(attribute.Int64("runny.runner.id", e.Runner.ID))
	})
}

func (a *traceAssembler) jobStarted(e obs.Event) {
	if e.Job == nil {
		return
	}
	a.withStep(e, func(cs *cycleSpans, ss *stepSpans) {
		ss.span.SetAttributes(attribute.String("runny.job.name", e.Job.Name))
	})
}

// jobEnded adds only what StepLeft doesn't carry: the job's operator-key
// audit. It lands on the root, not the JOB step span — a cycle has at most
// one job, the root is where "did this cycle's job run with a credential
// installed" is queried, and the root outlives any step-event ordering
// (a replayed journal from a daemon that emitted JobEnded after StepLeft
// still attaches it). The event's Outcome duplicates the step's own and is
// deliberately not re-attached.
func (a *traceAssembler) jobEnded(e obs.Event) {
	if e.Job == nil || len(e.Job.OperatorKeys) == 0 {
		return
	}
	a.withCycle(e, func(cs *cycleSpans) {
		cs.root.SetAttributes(attribute.StringSlice("runny.job.operator_keys", e.Job.OperatorKeys))
	})
}

func (a *traceAssembler) audit(e obs.Event) {
	if e.Audit == nil {
		return
	}
	a.withCycle(e, func(cs *cycleSpans) {
		// String fields attach unconditionally — an empty value is just
		// empty (a disarm entry has no fingerprint, a success no error).
		// Only the identity pair is conditional: nil OperatorUID with an
		// empty OperatorSID means the peer's identity could not be read —
		// absent attributes, distinct from a recorded uid 0 (root is a real
		// peer). A non-numeric identity attaches as operator_sid instead,
		// mirroring the audit record's own field pair.
		attrs := []attribute.KeyValue{
			attribute.String("runny.audit.fingerprint", e.Audit.Fingerprint),
			attribute.String("runny.audit.comment", e.Audit.Comment),
			attribute.String("runny.audit.reason", e.Audit.Reason),
			attribute.String("runny.audit.error", e.Audit.Error),
			attribute.String("runny.audit.outcome", e.Audit.Outcome),
			attribute.String("runny.audit.state", e.Audit.State),
			attribute.String("runny.audit.operator_user", e.Audit.OperatorUser),
		}
		if e.Audit.OperatorUID != nil {
			attrs = append(attrs, attribute.Int64("runny.audit.operator_uid", int64(*e.Audit.OperatorUID)))
		}
		if e.Audit.OperatorSID != "" {
			attrs = append(attrs, attribute.String("runny.audit.operator_sid", e.Audit.OperatorSID))
		}
		cs.root.AddEvent(string(e.Kind), trace.WithTimestamp(e.Time), trace.WithAttributes(attrs...))
	})
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

// pullStarted opens the pull's root span. If e.Pull.ID is already tracked,
// the entry there belongs to a predecessor puller for the same directory:
// internal/images/puller.go's registry only ever runs one live puller per
// dir, so a second KindPullStarted for the same ID means the predecessor
// has already been superseded — its own terminal event, whenever it
// arrives, will find ps.ref no longer matches (see getPull/resolveOwnPull)
// and safely no-op instead of touching this new entry. Close the stale
// entry's span right here instead of leaving it open forever waiting for a
// terminal event that can no longer reach it.
func (a *traceAssembler) pullStarted(e obs.Event) {
	if e.Pull == nil {
		return
	}
	ctx, span := a.tracer.Start(context.Background(), "runny.pull", trace.WithTimestamp(e.Time), trace.WithAttributes(
		attribute.String("runny.pull.id", e.Pull.ID),
		attribute.String("runny.image.ref", e.Pull.Ref),
		attribute.String("runny.image.digest", e.Pull.Digest),
	))
	a.mu.Lock()
	stale := a.pulls[e.Pull.ID]
	a.pulls[e.Pull.ID] = &pullSpans{ctx: ctx, root: span, ref: e.Pull}
	a.mu.Unlock()
	if stale == nil {
		return
	}
	stale.mu.Lock()
	defer stale.mu.Unlock()
	stale.root.SetStatus(codes.Error, "superseded by a new pull for the same image")
	stale.root.End(trace.WithTimestamp(e.Time))
}

// pullFinished ends the pull's root span with its outcome and drops its map
// entry. A puller cancelled before a terminal outcome never calls finish()
// and so never emits this — see pullAbandoned, the counterpart that closes
// the span for that path instead, so no entry is ever left un-evicted.
func (a *traceAssembler) pullFinished(e obs.Event) {
	if e.Pull == nil {
		return
	}
	ps := a.resolveOwnPull(e)
	if ps == nil {
		return
	}

	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.lastDetail != "" {
		ps.root.SetAttributes(attribute.String("runny.progress.last", ps.lastDetail))
	}
	if e.PullInfo != nil {
		ps.root.SetAttributes(
			attribute.String("runny.outcome", string(e.PullInfo.Outcome)),
			attribute.Int64("runny.pull.bytes", e.PullInfo.Bytes),
		)
		if outcomeIsFailure(e.PullInfo.Outcome) {
			ps.root.SetStatus(codes.Error, e.PullInfo.Error)
		}
	}
	ps.root.End(trace.WithTimestamp(e.Time))
}

// pullAbandoned ends the pull's root span for the one path pullFinished
// never sees: the last subscriber left before an attempt resolved, so
// internal/images/puller.go's run() returned without a terminal outcome to
// report. There is no outcome or bytes to attach — the pull's own work may
// still be salvageable by a successor puller for the same dir, this cycle's
// wait just stopped watching it — so the span closes as an error with a
// fixed reason instead of a fabricated result.
func (a *traceAssembler) pullAbandoned(e obs.Event) {
	if e.Pull == nil {
		return
	}
	ps := a.resolveOwnPull(e)
	if ps == nil {
		return
	}

	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.lastDetail != "" {
		ps.root.SetAttributes(attribute.String("runny.progress.last", ps.lastDetail))
	}
	ps.root.SetStatus(codes.Error, "abandoned: last subscriber left before a terminal outcome")
	ps.root.End(trace.WithTimestamp(e.Time))
}
