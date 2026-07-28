package vm

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bojanrajkovic/runny/internal/bounded"
)

// awaitBounded's whole reason to exist is the platform call that cannot be
// cancelled: HNS and VZ entry points take no context and have no internal
// timeout, so the only bound available is on OUR wait. These tests pin the
// three behaviours the call sites depend on.

func TestAwaitBoundedReturnsTheValue(t *testing.T) {
	ctx, cancel := bounded.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	got, err := awaitBounded(ctx, func() (int, error) { return 42, nil }, nil)
	if err != nil {
		t.Fatalf("awaitBounded: %v", err)
	}
	if got != 42 {
		t.Errorf("got %d, want 42", got)
	}
}

func TestAwaitBoundedPropagatesTheError(t *testing.T) {
	ctx, cancel := bounded.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	sentinel := errors.New("HNS said no")
	_, err := awaitBounded(ctx, func() (int, error) { return 0, sentinel }, nil)
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want the underlying error in the chain", err)
	}
}

// The deadline must bound OUR wait even though the call itself runs on
// regardless — that is the entire failure mode, since nothing can interrupt a
// wedged HNS RPC.
func TestAwaitBoundedReturnsWhenTheCallOutlivesTheDeadline(t *testing.T) {
	ctx, cancel := bounded.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	start := time.Now()
	_, err := awaitBounded(ctx, func() (int, error) {
		<-release // a wedged service: never returns on its own
		return 1, nil
	}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the deadline in the chain", err)
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Errorf("wait was not bounded: took %v", el)
	}
}

// A call that succeeds AFTER we gave up has produced a resource nobody holds:
// Boot has already returned an error, so the only reference is inside the
// goroutine. abandon is what disowns it — without it, a slow-but-healthy HNS
// leaks an endpoint per timed-out boot.
func TestAwaitBoundedAbandonsALateSuccess(t *testing.T) {
	ctx, cancel := bounded.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	release := make(chan struct{})
	abandoned := make(chan int, 1)

	if _, err := awaitBounded(ctx, func() (int, error) {
		<-release
		return 7, nil
	}, func(v int) { abandoned <- v }); err == nil {
		t.Fatal("expected the deadline error")
	}

	close(release) // the call finally resolves, long after we stopped waiting
	select {
	case got := <-abandoned:
		if got != 7 {
			t.Errorf("abandon got %d, want the value the call produced (7)", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a late success was never handed to abandon; the resource leaks")
	}
}

// A late FAILURE allocated nothing, so abandon must not run — calling it with a
// zero value would have the caller delete an endpoint that was never created.
func TestAwaitBoundedDoesNotAbandonALateFailure(t *testing.T) {
	ctx, cancel := bounded.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	release := make(chan struct{})
	abandoned := make(chan int, 1)

	if _, err := awaitBounded(ctx, func() (int, error) {
		<-release
		return 0, errors.New("failed after we gave up")
	}, func(v int) { abandoned <- v }); err == nil {
		t.Fatal("expected the deadline error")
	}

	close(release)
	select {
	case v := <-abandoned:
		t.Errorf("abandon ran for a call that allocated nothing (got %d)", v)
	case <-time.After(250 * time.Millisecond):
	}
}
