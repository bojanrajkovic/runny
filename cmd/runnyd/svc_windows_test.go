//go:build windows

package main

import (
	"context"
	"errors"
	"testing"

	"golang.org/x/sys/windows/svc"
)

func TestServiceExitCode(t *testing.T) {
	if svcSpecific, code := serviceExitCode(nil); !svcSpecific || code != 0 {
		t.Errorf("serviceExitCode(nil) = (%v, %d), want (true, 0)", svcSpecific, code)
	}
	if svcSpecific, code := serviceExitCode(errors.New("restarting after drain")); !svcSpecific || code != 1 {
		t.Errorf("serviceExitCode(err) = (%v, %d), want (true, 1)", svcSpecific, code)
	}
}

func TestIsStopCmd(t *testing.T) {
	cases := map[svc.Cmd]bool{
		svc.Stop:        true,
		svc.Shutdown:    true,
		svc.Interrogate: false,
		svc.Pause:       false,
		svc.Continue:    false,
	}
	for cmd, want := range cases {
		if got := isStopCmd(cmd); got != want {
			t.Errorf("isStopCmd(%v) = %v, want %v", cmd, got, want)
		}
	}
}

// TestServiceHandlerExecute drives serviceHandler against a fake run and fake
// SCM channels. It plays the role the real x/sys/windows/svc package's
// serviceMain plays in production: tracking the last status Execute sent so
// ChangeRequest.CurrentStatus is populated the way a live SCM would.
func TestServiceHandlerExecute(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	finishRun := make(chan struct{})
	fakeRun := func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		close(cancelled)
		// Held open until the test finishes its post-cancel assertions:
		// returning here makes done ready, and since reqs is unbuffered,
		// Execute's select could pick done over a still-pending reqs send
		// and return first, leaving that send blocked forever.
		<-finishRun
		return nil
	}
	h := &serviceHandler{run: fakeRun}

	reqs := make(chan svc.ChangeRequest)
	statuses := make(chan svc.Status, 8)
	type result struct {
		svcSpecific bool
		code        uint32
	}
	done := make(chan result, 1)
	go func() {
		svcSpecific, code := h.Execute(nil, reqs, statuses)
		done <- result{svcSpecific, code}
	}()

	if st := <-statuses; st.State != svc.StartPending {
		t.Fatalf("first status = %v, want StartPending", st.State)
	}
	<-started
	running := <-statuses
	if running.State != svc.Running || running.Accepts != svc.AcceptStop|svc.AcceptShutdown {
		t.Fatalf("second status = %+v, want Running with Accepts Stop|Shutdown", running)
	}

	// Interrogate before Stop echoes the last-announced (Running) status.
	reqs <- svc.ChangeRequest{Cmd: svc.Interrogate, CurrentStatus: running}
	if st := <-statuses; st.State != svc.Running {
		t.Fatalf("interrogate echo = %v, want Running", st.State)
	}

	reqs <- svc.ChangeRequest{Cmd: svc.Stop, CurrentStatus: running}
	stopping := <-statuses
	if stopping.State != svc.StopPending || stopping.CheckPoint != 1 {
		t.Fatalf("status after Stop = %+v, want StopPending with CheckPoint 1", stopping)
	}
	<-cancelled

	// A redundant Stop/Shutdown while already draining must not announce a
	// second StopPending — only the ticker (not exercised by this test)
	// advances the checkpoint from here.
	reqs <- svc.ChangeRequest{Cmd: svc.Shutdown, CurrentStatus: stopping}
	reqs <- svc.ChangeRequest{Cmd: svc.Interrogate, CurrentStatus: stopping}
	if st := <-statuses; st.State != svc.StopPending || st.CheckPoint != 1 {
		t.Fatalf("interrogate echo after redundant Shutdown = %+v, want StopPending with CheckPoint still 1", st)
	}
	close(finishRun)

	r := <-done
	if !r.svcSpecific || r.code != 0 {
		t.Fatalf("Execute returned (%v, %d), want (true, 0)", r.svcSpecific, r.code)
	}
}
