package main

import "io"

// launchContext is how runnyd was started, which on macOS decides whether it is
// auto-allowed Local Network (vmnet) access. The distinction is load-bearing for
// the never-self-daemonize invariant: a process started by launchd (agent or
// daemon, any uid) is auto-allowed; a foreground child of an interactive shell
// or sshd inherits that exemption; but a process that self-daemonizes — forks
// and lets its parent exit, reparenting to launchd — is neither, and macOS
// silently denies it vmnet access (guest dials fail "no route to host"). runnyd
// must never background itself; crash-only KeepAlive keeps it a launchd child.
// This type makes the invariant observable at startup, not only at the first
// failed guest dial.
type launchContext int

const (
	launchForeground launchContext = iota // real parent (shell / sshd child) — auto-allowed via the exemption
	launchLaunchd                         // started by launchd (agent or daemon) — auto-allowed
	launchOrphaned                        // reparented to launchd after self-daemonizing — silently denied
)

func (c launchContext) String() string {
	switch c {
	case launchLaunchd:
		return "launchd"
	case launchOrphaned:
		return "orphaned"
	default:
		return "foreground"
	}
}

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
// has no terminal to tee to.
func logSinkFor(lc launchContext, file, console io.Writer) io.Writer {
	if lc == launchForeground {
		return io.MultiWriter(file, console)
	}
	return file
}
