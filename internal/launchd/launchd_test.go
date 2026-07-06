package launchd

import (
	"context"
	"errors"
	"testing"
)

func TestClassify(t *testing.T) {
	// A zero exit (the job dump) is registered.
	if got := Classify("com.coderinserepeat.runnyd = {\n\tactive count = 1\n}", nil); got != Registered {
		t.Errorf("a job dump must be Registered, got %v", got)
	}
	// "could not find service" (reachable domain, label absent) is not-registered.
	if got := Classify(
		`Could not find service "com.coderinserepeat.runnyd" in domain for user gui: 501`,
		errors.New("exit status 113"),
	); got != NotRegistered {
		t.Errorf("service-not-found must be NotRegistered, got %v", got)
	}
	// "could not find domain" (user not logged in — no gui domain, nothing loaded) is
	// also not-registered.
	if got := Classify("Could not find domain for user gui: 501", errors.New("exit status 113")); got != NotRegistered {
		t.Errorf("domain-not-found must be NotRegistered, got %v", got)
	}
	// A permission denial is NOT an absence — it's Indeterminate, surfaced loudly.
	if got := Classify("Operation not permitted", errors.New("exit status 1")); got != Indeterminate {
		t.Errorf("a permission denial must be Indeterminate (fail-closed), got %v", got)
	}
	// A timeout/kill is Indeterminate, never a false absence.
	if got := Classify("", errors.New("signal: killed")); got != Indeterminate {
		t.Errorf("a killed probe must be Indeterminate, got %v", got)
	}
}

func TestProbeRunsThroughTheRunner(t *testing.T) {
	var gotTarget string
	run := func(_ context.Context, target string) (string, error) {
		gotTarget = target
		return "job = {}", nil
	}
	if got := Probe(context.Background(), run, "gui/501/com.coderinserepeat.runnyd"); got != Registered {
		t.Errorf("Probe must classify the runner's output, got %v", got)
	}
	if gotTarget != "gui/501/com.coderinserepeat.runnyd" {
		t.Errorf("Probe must pass the target through to the runner, got %q", gotTarget)
	}
}
