//go:build windows

package telemetry

import (
	"context"
	"crypto/rand"
	"fmt"
	"sync"

	octrace "go.opencensus.io/trace"

	"github.com/bojanrajkovic/runny/internal/obs"
)

// installOpenCensusAdapter routes go.opencensus.io/trace spans -- the
// vendored winhcs tree's own span API (internal/winhcs/oc) -- into runny's
// obs event stream, so the Hyper-V backend's compute-system operations land
// inside the cycle that caused them.
//
// It deliberately does NOT use go.opentelemetry.io/otel/bridge/opencensus,
// which is what this did first. That bridge converts each OpenCensus span
// into an OTel span directly, and an OTel span needs a parent in the
// caller's context to attach to. runny never puts one there: traces are
// assembled from the event stream (see trace.go), so a span handle lives in
// the assembler, never in an FSM context. Every bridged span therefore
// became its own root -- one disconnected trace per HCS call, roughly six
// per cycle. Emitting obs events instead lets the assembler parent them the
// same way it parents everything else.
//
// windows-only: winhcs is the only OpenCensus span producer in this
// codebase, and it is windows-only itself.
func installOpenCensusAdapter() {
	octrace.DefaultTracer = obsTracer{}
}

// obsSpanKey carries the innermost adapted span so a nested StartSpan can
// name it as its parent. This is what preserves the dependency's own
// nesting -- winhcs opens a wrapper span and then a syscall-level span
// inside it -- rather than flattening both onto the step.
type obsSpanKey struct{}

// ocSpanKey answers FromContext. The vendored oc.StartSpan calls
// log.UpdateContext, which looks the span up this way to stamp log lines.
type ocSpanKey struct{}

type obsTracer struct{}

func (t obsTracer) StartSpan(ctx context.Context, name string, _ ...octrace.StartOption) (context.Context, *octrace.Span) {
	var parentID string
	if p, ok := ctx.Value(obsSpanKey{}).(*obsSpan); ok {
		parentID = p.id
	}

	s := &obsSpan{ctx: ctx, id: newAdaptedSpanID(), name: name, sc: newAdaptedSpanContext()}
	if obs.Live(ctx) {
		obs.Emit(ctx, obs.Event{Kind: obs.KindBackendStarted, Backend: &obs.BackendEvent{
			ID:       s.id,
			ParentID: parentID,
			Name:     name,
			// The adapted span's own identity, so a winhcs log line -- which
			// is stamped with the OpenCensus span ID, not the OTel one the
			// assembler mints asynchronously -- can still be tied back to
			// the span it belongs to.
			Attrs: []obs.Attr{{Key: "runny.backend.span_id", Value: s.id}},
		}})
	}

	span := octrace.NewSpan(s)
	ctx = context.WithValue(ctx, obsSpanKey{}, s)
	ctx = context.WithValue(ctx, ocSpanKey{}, span)
	return ctx, span
}

// StartSpanWithRemoteParent delegates to StartSpan: nothing in the vendored
// tree crosses a process boundary, so there is no remote parent to honour.
func (t obsTracer) StartSpanWithRemoteParent(ctx context.Context, name string, _ octrace.SpanContext, o ...octrace.StartOption) (context.Context, *octrace.Span) {
	return t.StartSpan(ctx, name, o...)
}

func (obsTracer) FromContext(ctx context.Context) *octrace.Span {
	s, _ := ctx.Value(ocSpanKey{}).(*octrace.Span)
	return s
}

func (obsTracer) NewContext(parent context.Context, s *octrace.Span) context.Context {
	return context.WithValue(parent, ocSpanKey{}, s)
}

// obsSpan is one adapted span. Its End emits the closing half of the pair;
// everything else on octrace.SpanInterface either accumulates state that
// End reports or is a no-op, because obs has no equivalent concept.
type obsSpan struct {
	ctx  context.Context
	id   string
	name string
	sc   octrace.SpanContext

	mu     sync.Mutex
	attrs  []obs.Attr
	status octrace.Status
	ended  bool
}

// IsRecordingEvents reports true so the vendored oc.StartSpan still enriches
// its log context the way it does today -- that behaviour is gated on this.
func (s *obsSpan) IsRecordingEvents() bool { return true }

func (s *obsSpan) SpanContext() octrace.SpanContext { return s.sc }

func (s *obsSpan) End() {
	s.mu.Lock()
	// Guard against a double End: the vendored tree both defers span.End()
	// and, on some paths, ends explicitly. A second closing event would
	// reach an assembler entry that is already gone -- harmless, but there
	// is no reason to emit it.
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	status := s.status
	s.mu.Unlock()

	if !obs.Live(s.ctx) {
		return
	}
	outcome := obs.OutcomeOK
	if status.Code != 0 {
		outcome = obs.OutcomeError
	}
	obs.Emit(s.ctx, obs.Event{Kind: obs.KindBackendEnded, Backend: &obs.BackendEvent{
		ID:      s.id,
		Outcome: outcome,
		Error:   status.Message,
	}})
}

func (s *obsSpan) SetName(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.name = name
}

func (s *obsSpan) SetStatus(status octrace.Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
}

func (s *obsSpan) AddAttributes(attrs ...octrace.Attribute) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range attrs {
		s.attrs = append(s.attrs, obs.Attr{Key: a.Key(), Value: fmt.Sprint(a.Value())})
	}
}

func (s *obsSpan) String() string { return fmt.Sprintf("obsSpan(%s)", s.name) }

// The remainder of octrace.SpanInterface has no obs equivalent. Annotations
// and message events are deliberately dropped rather than mapped onto
// milestones: a milestone attaches to the currently open ACTION, and these
// fire from inside a backend span, so they would land on the wrong parent
// and read as progress the cycle never made.
func (s *obsSpan) Annotate([]octrace.Attribute, string)                  {}
func (s *obsSpan) Annotatef([]octrace.Attribute, string, ...interface{}) {}
func (s *obsSpan) AddMessageSendEvent(int64, int64, int64)               {}
func (s *obsSpan) AddMessageReceiveEvent(int64, int64, int64)            {}
func (s *obsSpan) AddLink(octrace.Link)                                  {}

func newAdaptedSpanID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%x", b)
}

// newAdaptedSpanContext mints identifiers for callers that read
// SpanContext (the vendored logging path). They are not the IDs of the OTel
// span the assembler eventually creates -- that one does not exist yet at
// StartSpan time -- which is why the adapted span's own ID is also carried
// as an attribute.
func newAdaptedSpanContext() octrace.SpanContext {
	var sc octrace.SpanContext
	_, _ = rand.Read(sc.TraceID[:])
	_, _ = rand.Read(sc.SpanID[:])
	sc.TraceOptions = octrace.TraceOptions(1)
	return sc
}
