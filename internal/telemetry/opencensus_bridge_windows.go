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

// spanKey carries the innermost adapted span. It does two jobs: a nested
// StartSpan reads it to name its parent, which is what preserves the
// dependency's own nesting (winhcs opens a wrapper span and then a
// syscall-level span inside it) rather than flattening both onto the step;
// and FromContext answers from the octrace.Span it holds.
type spanKey struct{}

// adapted is what rides under spanKey. s is nil for a context built by
// NewContext, which hands us only an octrace.Span with no way back to the
// obsSpan inside it -- such a context can answer FromContext but cannot
// parent a nested span.
type adapted struct {
	s  *obsSpan
	oc *octrace.Span
}

type obsTracer struct{}

func (t obsTracer) StartSpan(ctx context.Context, name string, _ ...octrace.StartOption) (context.Context, *octrace.Span) {
	var parentID string
	if a, ok := ctx.Value(spanKey{}).(adapted); ok && a.s != nil {
		parentID = a.s.id
	}

	s := &obsSpan{ctx: ctx, name: name, sc: newAdaptedSpanContext()}
	s.id = s.sc.SpanID.String()
	obs.Emit(ctx, obs.Event{Kind: obs.KindBackendStarted, Backend: &obs.BackendEvent{
		ID:       s.id,
		ParentID: parentID,
		Name:     name,
	}})

	span := octrace.NewSpan(s)
	return context.WithValue(ctx, spanKey{}, adapted{s: s, oc: span}), span
}

// StartSpanWithRemoteParent delegates to StartSpan: nothing in the vendored
// tree crosses a process boundary, so there is no remote parent to honour.
func (t obsTracer) StartSpanWithRemoteParent(ctx context.Context, name string, _ octrace.SpanContext, o ...octrace.StartOption) (context.Context, *octrace.Span) {
	return t.StartSpan(ctx, name, o...)
}

// FromContext and NewContext round-trip a span through a context. The
// vendored tree calls neither -- it threads the context StartSpan returns --
// so these exist to satisfy octrace.Tracer and are kept consistent with
// StartSpan's key rather than given a second one of their own.
func (obsTracer) FromContext(ctx context.Context) *octrace.Span {
	a, _ := ctx.Value(spanKey{}).(adapted)
	return a.oc
}

// NewContext must not call octrace.NewContext, which dispatches straight
// back here.
func (obsTracer) NewContext(parent context.Context, s *octrace.Span) context.Context {
	return context.WithValue(parent, spanKey{}, adapted{oc: s})
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
}

// IsRecordingEvents reports true so the vendored oc.StartSpan still enriches
// its log context the way it does today -- that behaviour is gated on this.
func (s *obsSpan) IsRecordingEvents() bool { return true }

func (s *obsSpan) SpanContext() octrace.SpanContext { return s.sc }

// End emits the closing event, carrying the attributes the dependency set
// after the span opened -- winhcs stamps the compute system id that way, and
// it is the only per-slot identifier these spans have.
func (s *obsSpan) End() {
	s.mu.Lock()
	attrs, status := s.attrs, s.status
	s.mu.Unlock()

	outcome := obs.OutcomeOK
	if status.Code != 0 {
		outcome = obs.OutcomeError
	}
	obs.Emit(s.ctx, obs.Event{Kind: obs.KindBackendEnded, Backend: &obs.BackendEvent{
		ID:      s.id,
		Attrs:   attrs,
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

// newAdaptedSpanContext mints identifiers for callers that read
// SpanContext. Its SpanID doubles as the adapted span's own ID, so there is
// one source of randomness per span rather than two.
func newAdaptedSpanContext() octrace.SpanContext {
	var sc octrace.SpanContext
	_, _ = rand.Read(sc.TraceID[:])
	_, _ = rand.Read(sc.SpanID[:])
	sc.TraceOptions = octrace.TraceOptions(1)
	return sc
}
