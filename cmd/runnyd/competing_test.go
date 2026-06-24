package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bojanrajkovic/runny/internal/home"
)

func TestParseRegistration(t *testing.T) {
	// A zero exit (the job dump) is registered.
	if got := parseRegistration("com.coderinserepeat.runnyd = {\n\tactive count = 1\n}", nil); got != regPresent {
		t.Errorf("a job dump must be regPresent, got %v", got)
	}
	// "could not find service" (reachable domain, label absent) is absent.
	if got := parseRegistration(
		`Could not find service "com.coderinserepeat.runnyd" in domain for user gui: 501`,
		errors.New("exit status 113"),
	); got != regAbsent {
		t.Errorf("service-not-found must be regAbsent, got %v", got)
	}
	// "could not find domain" (operator not logged in — no gui domain, so nothing
	// loaded to compete) is also absent.
	if got := parseRegistration("Could not find domain for user gui: 501", errors.New("exit status 113")); got != regAbsent {
		t.Errorf("domain-not-found must be regAbsent, got %v", got)
	}
	// A permission denial is NOT absent — it's could-not-determine, surfaced loudly.
	if got := parseRegistration("Operation not permitted", errors.New("exit status 1")); got != regUnknown {
		t.Errorf("a permission denial must be regUnknown (fail-closed), got %v", got)
	}
	// A timeout/kill is could-not-determine, never a false absent.
	if got := parseRegistration("", errors.New("signal: killed")); got != regUnknown {
		t.Errorf("a killed probe must be regUnknown, got %v", got)
	}
}

func TestCompetingRegistrationVerdict(t *testing.T) {
	const target = "gui/501/com.coderinserepeat.runnyd"

	// Per-user home: nothing to detect — passes.
	if c := competingRegistrationVerdict(false, false, "", regUnknown); !c.OK {
		t.Errorf("a per-user home must pass the check, got %+v", c)
	}

	// System home, operator unresolved: loud, not a silent pass.
	if c := competingRegistrationVerdict(true, false, "", regUnknown); c.OK {
		t.Errorf("an unresolved operator must fail loudly, got %+v", c)
	}

	// System home, a per-user agent registered: fails, naming the target + remediation.
	c := competingRegistrationVerdict(true, true, target, regPresent)
	if c.OK {
		t.Errorf("a registered per-user agent must fail the check, got %+v", c)
	}
	if !strings.Contains(c.Detail, target) || !strings.Contains(c.Detail, "bootout") {
		t.Errorf("the detail must name the target and the bootout remediation, got %q", c.Detail)
	}

	// System home, no per-user agent: passes.
	if c := competingRegistrationVerdict(true, true, target, regAbsent); !c.OK {
		t.Errorf("no competing agent must pass, got %+v", c)
	}

	// System home, probe inconclusive: fails loudly ("couldn't determine"), never a false pass.
	c = competingRegistrationVerdict(true, true, target, regUnknown)
	if c.OK {
		t.Errorf("an inconclusive probe must fail loudly, got %+v", c)
	}
	if !strings.Contains(strings.ToLower(c.Detail), "couldn't determine") &&
		!strings.Contains(strings.ToLower(c.Detail), "could not determine") {
		t.Errorf("the inconclusive detail must say it couldn't determine, got %q", c.Detail)
	}
}

func TestCheckCompetingRegistrationPerUserHomeSkips(t *testing.T) {
	// A per-user home resolves before any probe — no launchctl, no config stat.
	c := checkCompetingRegistration(context.Background(), home.Dir("/Users/someone/.runny"), "")
	if !c.OK {
		t.Errorf("a per-user home must skip cleanly, got %+v", c)
	}
}

func TestCheckCompetingRegistrationSystemHomeRegistered(t *testing.T) {
	orig := launchctlPrint
	defer func() { launchctlPrint = orig }()
	var probed string
	launchctlPrint = func(_ context.Context, target string) (string, error) {
		probed = target
		return "com.coderinserepeat.runnyd = {\n}", nil // registered
	}
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := checkCompetingRegistration(context.Background(), home.Dir(home.SystemHomeDir), cfgPath)
	if c.OK {
		t.Errorf("a registered per-user agent must fail the check, got %+v", c)
	}
	if !strings.HasPrefix(probed, "gui/") {
		t.Errorf("the probe must target the operator's gui/ domain, got %q", probed)
	}
}

func TestCheckCompetingRegistrationSystemHomeUnresolvedOperator(t *testing.T) {
	// A missing config.yaml means the operator can't be resolved — loud, never a
	// silent pass that would hide a competing agent.
	c := checkCompetingRegistration(
		context.Background(), home.Dir(home.SystemHomeDir), filepath.Join(t.TempDir(), "does-not-exist.yaml"),
	)
	if c.OK {
		t.Errorf("an unresolvable operator must fail loudly, got %+v", c)
	}
}
