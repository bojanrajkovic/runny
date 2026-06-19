package main

import (
	"bytes"
	"strings"
	"testing"

	runnyv1 "github.com/bojanrajkovic/runny/proto/runny/v1"
)

// classifyLaunchContext is the load-bearing discrimination behind the
// never-self-daemonize invariant. A spike confirmed launchd sets
// XPC_SERVICE_NAME to the job label for anything it starts, while every other
// process sees the sentinel "0" — so the label, not ppid==1, separates a
// launchd job from a self-daemonized orphan (both have ppid 1).
func TestClassifyLaunchContext(t *testing.T) {
	cases := []struct {
		name string
		ppid int
		xpc  string
		want launchContext
	}{
		{"launchd job: label, ppid 1", 1, "com.coderinserepeat.runnyd", launchLaunchd},
		{"launchd job: label even if parent isn't pid 1", 4321, "com.coderinserepeat.runnyd", launchLaunchd},
		{"orphaned: sentinel 0, ppid 1", 1, "0", launchOrphaned},
		{"orphaned: env unset, ppid 1", 1, "", launchOrphaned},
		{"foreground: sentinel 0, real parent", 40200, "0", launchForeground},
		{"foreground: env unset, real parent", 40200, "", launchForeground},
	}
	for _, tc := range cases {
		if got := classifyLaunchContext(tc.ppid, tc.xpc); got != tc.want {
			t.Errorf("%s: classifyLaunchContext(%d, %q) = %v, want %v",
				tc.name, tc.ppid, tc.xpc, got, tc.want)
		}
	}
}

// An orphaned (self-daemonized) runnyd must fail the local-network doctor check
// immediately — before any guest boots — so `runnyctl doctor` is red and the
// cause is named, rather than the operator discovering it as a "no route to
// host" only at the first guest dial.
func TestLocalNetworkReachFailsWhenOrphaned(t *testing.T) {
	defer func(orig func() launchContext) { launchContextNow = orig }(launchContextNow)
	launchContextNow = func() launchContext { return launchOrphaned }
	ok, detail := localNetworkReach()
	if ok {
		t.Fatal("an orphaned/self-daemonized runnyd must fail the local-network check")
	}
	if !strings.Contains(detail, "launchd") {
		t.Errorf("detail should name the launchd/self-daemonize cause: %q", detail)
	}
}

// The orphaned context must report UNKNOWN, never DENIED — unconditionally, even
// once a vmnet interface is up (the live probe would otherwise return DENIED).
// runnyctl and the app render DENIED as "grant Local Network in System Settings",
// a dead end for a self-daemonized daemon (Codex review, #111). The cause is
// surfaced via the local-network doctor check + the startup log; a distinct
// client-side grant state is app-track work (#112).
func TestLocalNetworkGrantUnknownWhenOrphaned(t *testing.T) {
	defer func(orig func() launchContext) { launchContextNow = orig }(launchContextNow)
	launchContextNow = func() launchContext { return launchOrphaned }
	if got := localNetworkGrant(); got != runnyv1.LocalNetworkGrant_LOCAL_NETWORK_GRANT_UNKNOWN {
		t.Errorf("orphaned grant = %v, want UNKNOWN (never DENIED — misleads the TCC remediation; cause is on the doctor check, app-track #112)", got)
	}
}

// Foreground and launchd contexts must NOT trip the orphaned branch — the check
// falls through to the normal vmnet probe (which on a host without a vmnet
// interface reports informational-green / UNKNOWN, exactly as before).
func TestLocalNetworkNotOrphanedFallsThrough(t *testing.T) {
	if vmnetInterfaceUp() {
		t.Skip("a 192.168.64.0/24 interface is present; the live probe would run")
	}
	defer func(orig func() launchContext) { launchContextNow = orig }(launchContextNow)
	launchContextNow = func() launchContext { return launchForeground }
	if ok, detail := localNetworkReach(); !ok {
		t.Errorf("foreground without vmnet must not fail: %q", detail)
	}
	if got := localNetworkGrant(); got != runnyv1.LocalNetworkGrant_LOCAL_NETWORK_GRANT_UNKNOWN {
		t.Errorf("foreground grant without vmnet = %v, want UNKNOWN", got)
	}
}

// A foreground runnyd tees its structured log to the console so a dev running it
// over SSH sees output live; a launchd-started or orphaned runnyd writes to the
// file only (launchd captures its own stdout/stderr; an orphan has no terminal).
func TestLogSinkForTeesOnlyForeground(t *testing.T) {
	write := func(lc launchContext) (file, console *bytes.Buffer) {
		file, console = &bytes.Buffer{}, &bytes.Buffer{}
		if _, err := logSinkFor(lc, file, console).Write([]byte("line\n")); err != nil {
			t.Fatalf("write: %v", err)
		}
		return file, console
	}
	if file, console := write(launchForeground); file.Len() == 0 || console.Len() == 0 {
		t.Errorf("foreground must tee to both: file=%d console=%d", file.Len(), console.Len())
	}
	if file, console := write(launchLaunchd); file.Len() == 0 || console.Len() != 0 {
		t.Errorf("launchd must write file only: file=%d console=%d", file.Len(), console.Len())
	}
	if _, console := write(launchOrphaned); console.Len() != 0 {
		t.Errorf("orphaned must not tee to console: console=%d", console.Len())
	}
}
