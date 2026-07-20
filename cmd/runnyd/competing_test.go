package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
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

	// System home, operator unresolved: loud, not a silent pass.
	if c := competingRegistrationVerdict(true, false, "", launchd.Indeterminate); c.OK {
		t.Errorf("an unresolved operator must fail loudly, got %+v", c)
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

func TestCheckCompetingRegistrationPerUserHomeSkips(t *testing.T) {
	// A per-user home resolves before any probe — no launchctl, no config stat.
	c := checkCompetingRegistration(context.Background(), home.Dir("/Users/someone/.runny"), "")
	if !c.OK {
		t.Errorf("a per-user home must skip cleanly, got %+v", c)
	}
}

func TestCheckCompetingRegistrationSystemHomeRegistered(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("checkCompetingRegistration is entirely macOS-launchd-specific (gui/ domains); fileOwnerUID's windows stub always errors, so this probe path can't be exercised here")
	}
	orig := launchdRunner
	defer func() { launchdRunner = orig }()
	var probed string
	launchdRunner = func(_ context.Context, target string) (string, error) {
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
