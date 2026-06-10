// Package bounded makes runny's core invariant — no operation is ever
// unbounded — a property of the type system instead of a code-review
// convention (ADR-0011). A bounded.Context can only be obtained from a
// constructor that attaches a bound: a wall-clock deadline (WithTimeout,
// WithDeadline) or a progress watcher (Stall.Watch). Functions that talk to
// a network or a guest take bounded.Context, so an unbounded call site is a
// compile error, not a silent production hang.
package bounded

import (
	"context"
	"time"
)

// Context is a context.Context that carries a bound. The inner context is
// unexported on purpose: bounded.Context{...} cannot be built outside this
// package, so a daemon-lifetime context cannot be laundered into a
// bounded-looking one. The zero value is invalid; always use a constructor.
type Context struct {
	ctx context.Context
}

var _ context.Context = Context{}

func (c Context) Deadline() (time.Time, bool) { return c.ctx.Deadline() }
func (c Context) Done() <-chan struct{}       { return c.ctx.Done() }
func (c Context) Err() error                  { return c.ctx.Err() }
func (c Context) Value(key any) any           { return c.ctx.Value(key) }

// WithTimeout bounds parent by a wall-clock duration.
func WithTimeout(parent context.Context, d time.Duration) (Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(parent, d)
	return Context{ctx: ctx}, cancel
}

// WithDeadline bounds parent by an absolute deadline.
func WithDeadline(parent context.Context, t time.Time) (Context, context.CancelFunc) {
	ctx, cancel := context.WithDeadline(parent, t)
	return Context{ctx: ctx}, cancel
}
