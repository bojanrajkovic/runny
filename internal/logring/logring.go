// Package logring is the daemon's log fan-out: every record goes to the
// structured file sink and into an in-memory ring buffer that StreamLogs
// replays and follows. Diagnosability is not optional (pillar: no silent
// anything).
package logring

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"
)

// Entry is one captured log record (the wire shape of runny.v1.LogLine).
type Entry struct {
	Time    time.Time
	Level   string
	Message string
	Attrs   map[string]string
}

// Ring is a fixed-size record buffer with subscriber fan-out.
type Ring struct {
	mu     sync.Mutex
	buf    []Entry
	size   int
	subs   map[int]chan Entry
	nextID int
}

func NewRing(size int) *Ring {
	return &Ring{size: size, subs: map[int]chan Entry{}}
}

func (r *Ring) add(e Entry) {
	r.mu.Lock()
	r.buf = append(r.buf, e)
	if len(r.buf) > r.size {
		r.buf = r.buf[len(r.buf)-r.size:]
	}
	for _, ch := range r.subs {
		select {
		case ch <- e:
		default: // slow subscriber loses lines rather than wedging logging
		}
	}
	r.mu.Unlock()
}

// Subscribe returns a channel that replays the last `replay` entries then
// follows. Call the returned cancel to unsubscribe.
func (r *Ring) Subscribe(replay int) (<-chan Entry, func()) {
	r.mu.Lock()
	id := r.nextID
	r.nextID++
	ch := make(chan Entry, 256)
	start := max(len(r.buf)-replay, 0)
	if replay > 0 {
		for _, e := range r.buf[start:] {
			ch <- e
		}
	}
	r.subs[id] = ch
	r.mu.Unlock()
	return ch, func() {
		r.mu.Lock()
		delete(r.subs, id)
		r.mu.Unlock()
	}
}

// Handler tees slog records into the ring and a wrapped handler (the file
// sink).
type Handler struct {
	inner slog.Handler
	ring  *Ring
	attrs []slog.Attr
	group string
}

func NewHandler(w io.Writer, level slog.Level, ring *Ring) *Handler {
	return &Handler{
		inner: slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level}),
		ring:  ring,
	}
}

func (h *Handler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *Handler) Handle(ctx context.Context, rec slog.Record) error {
	attrs := make(map[string]string, rec.NumAttrs()+len(h.attrs))
	for _, a := range h.attrs {
		attrs[h.qualify(a.Key)] = a.Value.String()
	}
	rec.Attrs(func(a slog.Attr) bool {
		attrs[h.qualify(a.Key)] = a.Value.String()
		return true
	})
	h.ring.add(Entry{
		Time:    rec.Time,
		Level:   rec.Level.String(),
		Message: rec.Message,
		Attrs:   attrs,
	})
	return h.inner.Handle(ctx, rec)
}

func (h *Handler) qualify(key string) string {
	if h.group == "" {
		return key
	}
	return h.group + "." + key
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &Handler{
		inner: h.inner.WithAttrs(attrs),
		ring:  h.ring,
		attrs: append(append([]slog.Attr{}, h.attrs...), attrs...),
		group: h.group,
	}
}

func (h *Handler) WithGroup(name string) slog.Handler {
	g := name
	if h.group != "" {
		g = h.group + "." + name
	}
	return &Handler{inner: h.inner.WithGroup(name), ring: h.ring, attrs: h.attrs, group: g}
}

var _ slog.Handler = (*Handler)(nil)

// Verify Entry formats reasonably for human rendering.
func (e Entry) String() string {
	return fmt.Sprintf("%s %-5s %s", e.Time.Format(time.RFC3339), e.Level, e.Message)
}
