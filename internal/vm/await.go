package vm

import (
	"log/slog"

	"github.com/bojanrajkovic/runny/internal/bounded"
)

// awaitBounded runs a platform call that takes no context under one that does.
//
// The backends' host bindings are synchronous RPCs with no ctx and no internal
// timeout: HNS's hcn entry points, and VZ's own start/stop on darwin. A wedged
// service therefore blocks the calling goroutine indefinitely — and since that
// goroutine is the FSM's, it blows through whatever deadline the state was
// given, which is the failure this project exists to make impossible. Nothing
// can interrupt the call itself, so the only bound available is on OUR wait.
//
// abandon is the half that is easy to forget and expensive to omit. A call that
// resolves AFTER we gave up has produced something nobody holds a reference to,
// because the caller already returned an error; abandon runs detached and owns
// it. A call that FAILED late allocated nothing, so abandon is not called —
// handing it a zero value would have the caller release a resource that never
// existed. Pass nil when the call allocates nothing, which is the common case
// for pure reads.
//
// ponytail: a call that never resolves leaks its goroutine for the process's
// life. That is the deliberate trade — a wedged host binding cannot be
// cancelled, so the alternative is blocking the FSM instead, and one goroutine
// per attempt against a service that is down is the cheaper failure. The same
// ceiling applies to the existing abandoned-boot paths on both backends.
func awaitBounded[T any](ctx bounded.Context, fn func() (T, error), abandon func(T)) (T, error) {
	type result struct {
		v   T
		err error
	}
	ch := make(chan result, 1)
	go func() { v, err := fn(); ch <- result{v, err} }()

	var zero T
	select {
	case r := <-ch:
		if r.err != nil {
			return zero, r.err
		}
		return r.v, nil
	case <-ctx.Done():
		// Always drain, even with no abandon: the late outcome is the only way
		// to tell "the call landed a moment later" from "it really did leak",
		// and the caller's own error says only that WE stopped waiting.
		go func() {
			switch r := <-ch; {
			case r.err != nil:
				slog.Debug("bounded call failed after its deadline passed", "err", r.err)
			case abandon != nil:
				abandon(r.v)
			default:
				slog.Debug("bounded call succeeded after its deadline passed")
			}
		}()
		return zero, ctx.Err()
	}
}

// awaitBoundedErr is awaitBounded for a call that returns only an error, which
// is every release (a delete, a close): there is no value to disown, so no
// abandon. Takes the method value directly -- awaitBoundedErr(ctx, ep.Delete).
func awaitBoundedErr(ctx bounded.Context, fn func() error) error {
	_, err := awaitBounded(ctx, func() (struct{}, error) { return struct{}{}, fn() }, nil)
	return err
}
