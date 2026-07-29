package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/bojanrajkovic/runny/internal/home"
	"github.com/bojanrajkovic/runny/internal/launchd"
)

func TestCompetingRegistrationVerdict(t *testing.T) {
	const target = "gui/501/com.coderinserepeat.runnyd"

	// Per-user home: nothing to detect — passes.
	if c := competingRegistrationVerdict(false, false, "", launchd.Indeterminate); !c.OK {
		t.Errorf("a per-user home must pass the check, got %+v", c)
	}

	// System home, no operators resolved: loud, not a silent pass.
	if c := competingRegistrationVerdict(true, false, "", launchd.Indeterminate); c.OK {
		t.Errorf("an unresolved operator set must fail loudly, got %+v", c)
	}

	// System home, a per-user agent registered: fails, naming the target + remediation.
	c := competingRegistrationVerdict(true, true, target, launchd.Registered)
	if c.OK {
		t.Errorf("a registered per-user agent must fail the check, got %+v", c)
	}
	if !strings.Contains(c.Detail, target) || !strings.Contains(c.Detail, "bootout") {
		t.Errorf("the detail must name the target and the bootout remediation, got %q", c.Detail)
	}

	// System home, no per-user agent: passes.
	if c := competingRegistrationVerdict(true, true, target, launchd.NotRegistered); !c.OK {
		t.Errorf("no competing agent must pass, got %+v", c)
	}

	// System home, probe inconclusive: fails loudly ("couldn't determine"), never a false pass.
	c = competingRegistrationVerdict(true, true, target, launchd.Indeterminate)
	if c.OK {
		t.Errorf("an inconclusive probe must fail loudly, got %+v", c)
	}
	if !strings.Contains(strings.ToLower(c.Detail), "couldn't determine") {
		t.Errorf("the inconclusive detail must say it couldn't determine, got %q", c.Detail)
	}
}

// stubOperators replaces the operator-registry read for the duration of a test.
func stubOperators(t *testing.T, ids []string, err error) {
	t.Helper()
	orig := operatorIDs
	t.Cleanup(func() { operatorIDs = orig })
	operatorIDs = func(string) ([]string, error) { return ids, err }
}

// stubLaunchd answers each gui/ target from a map; anything absent is treated as
// a clean absence, so a test only spells out the domains it cares about.
func stubLaunchd(t *testing.T, byTarget map[string]launchd.Result) *[]string {
	t.Helper()
	orig := launchdRunner
	t.Cleanup(func() { launchdRunner = orig })
	var probed []string
	launchdRunner = func(_ context.Context, target string) (string, error) {
		probed = append(probed, target)
		// Two-value lookup on purpose: Indeterminate is the zero value, so a
		// plain index cannot tell "absent from the map" from "denied probe".
		res, ok := byTarget[target]
		if !ok {
			res = launchd.NotRegistered
		}
		switch res {
		case launchd.Registered:
			return "com.coderinserepeat.runnyd = {\n}", nil
		case launchd.Indeterminate:
			// Classify keys on stdout, so an error with no recognizable output
			// is what a denied or wedged probe actually looks like.
			return "", fmt.Errorf("probe denied")
		default:
			return `Could not find service "com.coderinserepeat.runnyd" in domain`, fmt.Errorf("exit 113")
		}
	}
	return &probed
}

func TestCheckCompetingRegistrationPerUserHomeSkips(t *testing.T) {
	// A per-user home resolves before any probe — no launchctl, no registry read.
	c := checkCompetingRegistration(context.Background(), home.Dir("/Users/someone/.runny"))
	if !c.OK {
		t.Errorf("a per-user home must skip cleanly, got %+v", c)
	}
}

// The case the old model could not express. A stale agent lives in ONE human's
// gui domain, and nothing says which — so every operator has to be probed.
// Keying on a single account (the owner of config.yaml, as this once did) reports
// a confident green while the conflict sits in the second operator's domain.
func TestCheckCompetingRegistrationFindsASecondOperatorsAgent(t *testing.T) {
	stubOperators(t, []string{"501", "502"}, nil)
	probed := stubLaunchd(t, map[string]launchd.Result{
		"gui/502/com.coderinserepeat.runnyd": launchd.Registered,
	})

	c := checkCompetingRegistration(context.Background(), home.Dir(home.SystemHomeDir))
	if c.OK {
		t.Fatalf("a registered agent in the second operator's domain must fail the check, got %+v", c)
	}
	if !strings.Contains(c.Detail, "gui/502/") {
		t.Errorf("the detail must name the domain that actually holds the agent, got %q", c.Detail)
	}
	if len(*probed) != 2 {
		t.Errorf("probed %v, want every operator's domain probed until one hits", *probed)
	}
}

// Every operator clean is the only way to a green verdict.
func TestCheckCompetingRegistrationAllOperatorsClean(t *testing.T) {
	stubOperators(t, []string{"501", "502", "503"}, nil)
	probed := stubLaunchd(t, nil)

	if c := checkCompetingRegistration(context.Background(), home.Dir(home.SystemHomeDir)); !c.OK {
		t.Errorf("no competing agent anywhere must pass, got %+v", c)
	}
	if len(*probed) != 3 {
		t.Errorf("probed %v, want all three operators probed before declaring a clean absence", *probed)
	}
}

// One denied probe among otherwise-clean operators is NOT a clean absence: the
// agent could be in exactly the domain that would not answer.
func TestCheckCompetingRegistrationInconclusiveProbeIsNotGreen(t *testing.T) {
	stubOperators(t, []string{"501", "502"}, nil)
	stubLaunchd(t, map[string]launchd.Result{
		"gui/502/com.coderinserepeat.runnyd": launchd.Indeterminate,
	})

	c := checkCompetingRegistration(context.Background(), home.Dir(home.SystemHomeDir))
	if c.OK {
		t.Fatalf("an inconclusive probe must not pass, got %+v", c)
	}
	if !strings.Contains(c.Detail, "gui/502/") {
		t.Errorf("the detail must name the domain that could not be determined, got %q", c.Detail)
	}
}

// An unreadable or empty operator set is loud. It is also what a daemon whose
// home ACL has been damaged looks like, which is worth hearing about.
func TestCheckCompetingRegistrationEmptyOperatorSetIsLoud(t *testing.T) {
	for _, tc := range []struct {
		name string
		ids  []string
		err  error
	}{
		{"empty", nil, nil},
		{"unreadable", nil, fmt.Errorf("reading the home ACL: denied")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubOperators(t, tc.ids, tc.err)
			stubLaunchd(t, nil)
			if c := checkCompetingRegistration(context.Background(), home.Dir(home.SystemHomeDir)); c.OK {
				t.Errorf("an unresolvable operator set must fail loudly, got %+v", c)
			}
		})
	}
}
