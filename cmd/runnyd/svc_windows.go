//go:build windows

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	"golang.org/x/sys/windows/svc"

	"github.com/bojanrajkovic/runny/internal/home"
)

// runEntry is main's platform seam. A console run (elevated or not)
// behaves exactly like run always has; started by the Service Control
// Manager, runnyd must speak the SCM protocol or it gets killed at startup.
//
// One known gap: run's pre-context startup work (config load, the vms-dir
// sweep, buildClients — none of it network-bounded) runs before parent is
// ever consulted, so an SCM Stop arriving in that narrow window has nowhere
// to land until it finishes on its own. Off-Windows a bare SIGTERM during
// the equivalent window kills the process outright via the OS's default
// disposition instead — neither is the graceful path; this one just fails
// differently. Not fixed here: doing so would mean threading a context
// through run's whole startup gauntlet, well beyond wrapping its entry path.
func runEntry() error {
	isService, err := svc.IsWindowsService()
	if err != nil {
		return fmt.Errorf("checking windows service context: %w", err)
	}
	if !isService {
		return run(context.Background())
	}
	if err := redirectServiceOutput(); err != nil {
		return fmt.Errorf("redirecting service output: %w", err)
	}
	h := &serviceHandler{run: run}
	if err := svc.Run("runnyd", h); err != nil {
		return fmt.Errorf("svc.Run: %w", err)
	}
	return h.runErr
}

// redirectServiceOutput points os.Stdout/os.Stderr at logs\service.out.log
// and logs\service.err.log under the resolved home, before run does
// anything of its own (including its own home resolution) — the SCM
// captures nothing, so this is the only way an SCM-started runnyd leaves a
// trail, the same visibility launchd's StandardOut/ErrorPath gives on
// macOS. detectLaunchContext (launchcontext_windows.go) keeps run's own
// structured-log tee from also writing into service.err.log — without that,
// the tee would double the full log stream into a file this redirect
// deliberately leaves uncapped (it's meant for rare early/crash output, not
// a second copy of runnyd.log). debug.SetCrashOutput additionally catches
// what os.Stderr reassignment alone can't: an unrecovered panic, which the
// Go runtime writes to its own cached OS stderr handle, bypassing the
// os.Stderr variable entirely. A failure here fails the service start
// loudly: svc.Run is never called, so the SCM sees the process exit without
// ever reaching StartServiceCtrlDispatcher — a start failure, not a silent
// hang.
func redirectServiceOutput() error {
	dir, err := home.ResolveServer()
	if err != nil {
		return fmt.Errorf("resolving home: %w", err)
	}
	if err := dir.Ensure(); err != nil {
		return err
	}
	out, err := os.OpenFile(filepath.Join(dir.LogsDir(), "service.out.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("opening service.out.log: %w", err)
	}
	errFile, err := os.OpenFile(filepath.Join(dir.LogsDir(), "service.err.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		_ = out.Close()
		return fmt.Errorf("opening service.err.log: %w", err)
	}
	os.Stdout = out
	os.Stderr = errFile
	if err := debug.SetCrashOutput(errFile, debug.CrashOptions{}); err != nil {
		return fmt.Errorf("setting crash output: %w", err)
	}
	return nil
}

// serviceHandler drives runnyd under the Windows SCM: report StartPending,
// launch run against a context this handler owns and can cancel, report
// Running, then translate SCM commands and run's own completion into the
// SCM protocol. run is injectable so tests drive the handler's decision
// logic against a fake without a live SCM.
type serviceHandler struct {
	run    func(context.Context) error
	runErr error
}

// stopCheckpointInterval paces the CheckPoint bumps Execute sends while
// draining: run's graceful shutdown (waiting out a running job, or the
// drainer's exit-gate holding on a bad config) can take far longer than a
// single StopPending report covers, and the SCM/OS shutdown timeout force-
// kills a service whose CheckPoint stops advancing. Comfortably under any
// realistic WaitToKillServiceTimeout, so several checkpoints land before one
// could ever be read as hung.
const stopCheckpointInterval = 10 * time.Second

func (h *serviceHandler) Execute(args []string, r <-chan svc.ChangeRequest, s chan<- svc.Status) (svcSpecificEC bool, exitCode uint32) {
	s <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.run(ctx) }()

	s <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}

	stopping := false
	var checkpoint uint32
	stopStatus := func() svc.Status {
		checkpoint++
		return svc.Status{State: svc.StopPending, CheckPoint: checkpoint, WaitHint: uint32(2 * stopCheckpointInterval / time.Millisecond)}
	}

	ticker := time.NewTicker(stopCheckpointInterval)
	defer ticker.Stop()
	for {
		select {
		case c := <-r:
			switch {
			case c.Cmd == svc.Interrogate:
				s <- c.CurrentStatus
			case isStopCmd(c.Cmd) && !stopping:
				// The SAME cancellation SIGTERM drives off-Windows: run
				// treats it as a graceful, non-restart-worthy shutdown. A
				// redundant Stop/Shutdown while already draining is a safe
				// no-op — the ticker below keeps reporting progress either way.
				stopping = true
				cancel()
				s <- stopStatus()
			}
		case <-ticker.C:
			if stopping {
				s <- stopStatus()
			}
		case h.runErr = <-done:
			return serviceExitCode(h.runErr)
		}
	}
}

// isStopCmd reports whether cmd is one of the two commands Execute
// advertises accepting (Accepts: Stop|Shutdown).
func isStopCmd(cmd svc.Cmd) bool {
	return cmd == svc.Stop || cmd == svc.Shutdown
}

// serviceExitCode maps run's returned error to the SCM exit-code contract.
// nil is a graceful Stop/Shutdown (mirroring SIGTERM off-Windows) and must
// report a clean exit so RecoveryActionsOnNonCrashFailures does not fire;
// any error — including the drain's deliberate non-zero cold-restart exit —
// must report a non-zero service-specific code so it does.
func serviceExitCode(err error) (svcSpecificEC bool, exitCode uint32) {
	if err == nil {
		return true, 0
	}
	return true, 1
}
