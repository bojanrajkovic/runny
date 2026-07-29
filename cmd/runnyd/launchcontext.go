package main

import "io"

// launchContext is how runnyd was started, which on macOS decides whether it is
// auto-allowed Local Network (vmnet) access. The distinction is load-bearing for
// the never-self-daemonize invariant: a process started by launchd (agent or
// daemon, any uid) is auto-allowed; a foreground child of an interactive shell
// or sshd inherits that exemption; but a process that self-daemonizes — forks
// and lets its parent exit, reparenting to launchd — is neither, and macOS
// silently denies it vmnet access (guest dials fail "no route to host"). runnyd
// must never background itself; KeepAlive restarts keep it a launchd child.
// This type makes the invariant observable at startup, not only at the first
// failed guest dial.
type launchContext int

const (
	launchForeground launchContext = iota // real parent (shell / sshd child) — auto-allowed via the exemption
	launchLaunchd                         // started by launchd (agent or daemon) — auto-allowed
	launchOrphaned                        // reparented to launchd after self-daemonizing — silently denied
	launchService                         // started by the Windows SCM — no vmnet meaning (windows-only), only logSinkFor cares
)

// orphanedDenyDetail explains the self-daemonized state wherever it surfaces —
// the local-network doctor check and the startup log. It names both the cause
// and the fix, since this is exactly the silent failure runny exists to kill.
const orphanedDenyDetail = "self-daemonized: launchd did not start this process and it has no live parent, " +
	"so macOS silently denies Local Network access (guest dials fail \"no route to host\"); " +
	"start runnyd via launchd or run it in the foreground, never background it"

// launchContextNow reads this process's launch context. It is a package var so
// the doctor/status checks can be exercised against each context in tests
// without spawning real processes; production code never reassigns it.
var launchContextNow = detectLaunchContext

// classifyLaunchContext is the pure decision behind detectLaunchContext, split
// out for exhaustive testing. launchd sets XPC_SERVICE_NAME to the job's label
// for anything it starts; every other process sees the sentinel "0" (or
// nothing). So a non-sentinel XPC_SERVICE_NAME is the definitive "started by
// launchd" signal — it alone separates a launchd job (ppid 1, label) from a
// self-daemonized orphan (ppid 1, "0"), which a bare ppid==1 check cannot. With
// no launchd label, ppid==1 is the orphaned signature: at startup a live
// foreground parent would still be attached, so a process already reparented to
// launchd (pid 1) must have been forked off and abandoned.
func classifyLaunchContext(ppid int, xpcServiceName string) launchContext {
	if xpcServiceName != "" && xpcServiceName != "0" {
		return launchLaunchd
	}
	if ppid == 1 {
		return launchOrphaned
	}
	return launchForeground
}

// logSinkFor decides where the structured log is written. A foreground runnyd
// (started from a shell or over SSH) tees to the console so the operator sees it
// live — today a foregrounded daemon prints nothing. A launchd-started or
// orphaned runnyd writes to the file only: launchd captures its own
// stdout/stderr separately (duplicating there would double-log), and an orphan
// has no terminal to tee to. Same reasoning for launchService: the SCM's
// redirected service.out/err.log is that platform's StandardOut/ErrorPath
// equivalent, already capturing console output — tee-ing the full structured
// log into it too would double-write an otherwise-uncapped file forever.
func logSinkFor(lc launchContext, file, console io.Writer) io.Writer {
	if lc == launchForeground {
		return io.MultiWriter(file, console)
	}
	return file
}

// openLogSink opens the structured log's destination. -doctor gets the console
// and NO file, which is what makes the documented read-only contract true:
//
//   - Against another deployment's home (-doctor -config) opening the log
//     creates it, re-modes it, appends, and rotates it at the cap — four writes
//     into a home the invoker does not own. For a non-owning operator the create
//     and the chmod simply fail, so the diagnostic never ran at all.
//   - Against the invoker's OWN home it is worse than a contract violation: a
//     -doctor pass deliberately skips the instance lock, whose stated job is
//     keeping a second process's startup lines out of the winner's log. Opening
//     that same log unlocked is precisely the interleave the lock exists to
//     prevent, and a rotation would rename it out from under the live daemon.
//
// Nothing is lost by dropping it: under -doctor the file sink carries one line
// (`runnyd starting`), the check table goes to stdout, and the operator is at a
// terminal by construction.
func openLogSink(checkOnly bool, lc launchContext, path string, console io.Writer) (io.Writer, io.Closer, error) {
	if checkOnly {
		return console, noopCloser{}, nil
	}
	f, err := openRotatingFile(path, logFileCap)
	if err != nil {
		return nil, nil, err
	}
	return logSinkFor(lc, f, console), f, nil
}

// noopCloser is the doctor sink's Close — there is no file to close — so the
// caller defers one unconditional Close either way.
type noopCloser struct{}

func (noopCloser) Close() error { return nil }
