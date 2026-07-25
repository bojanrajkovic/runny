package main

import (
	"testing"

	"github.com/bojanrajkovic/runny/internal/home"
)

// The known #351 bug: an operator's own ~/.runny doesn't own SystemHomeDir,
// so ResolveServer resolves them to their per-user home. Pointing -doctor
// straight at the system config must still diagnose the system home — not
// the invoker's own — since config.yaml always lives at <home>/config.yaml.
// It must also report diagnosingOther=true, so the caller skips Dir.Ensure
// rather than scaffolding another deployment's home.
func TestDoctorHomeExplicitConfigPinsHome(t *testing.T) {
	invoker := home.Dir("/Users/someone/.runny")
	configFlag := home.SystemHomeDir + "/config.yaml"
	dir, diagnosingOther := doctorHome(true, configFlag, invoker)
	if dir != home.Dir(home.SystemHomeDir) {
		t.Fatalf("doctorHome(explicit system -config) dir = %q, want %q", dir, home.SystemHomeDir)
	}
	if !diagnosingOther {
		t.Fatal("doctorHome(explicit system -config) diagnosingOther = false, want true")
	}
}

// A bare `-doctor` (no -config) keeps whatever ResolveServer already picked
// — that ownership-based fallback already describes an operator's own
// per-user daemon correctly beside an installed system home — and must not
// suppress the existing Dir.Ensure() call for that home.
func TestDoctorHomeNoConfigKeepsResolved(t *testing.T) {
	resolved := home.Dir("/Users/someone/.runny")
	dir, diagnosingOther := doctorHome(true, "", resolved)
	if dir != resolved {
		t.Fatalf("doctorHome(no -config) dir = %q, want %q", dir, resolved)
	}
	if diagnosingOther {
		t.Fatal("doctorHome(no -config) diagnosingOther = true, want false")
	}
}

// Outside doctor mode, -config must never change which home the real daemon
// binds/writes as — it only selects which config.yaml to load — and Ensure
// must still run for it.
func TestDoctorHomeRealDaemonUnaffected(t *testing.T) {
	resolved := home.Dir("/Users/someone/.runny")
	dir, diagnosingOther := doctorHome(false, home.SystemHomeDir+"/config.yaml", resolved)
	if dir != resolved {
		t.Fatalf("doctorHome(real daemon, explicit -config) dir = %q, want %q", dir, resolved)
	}
	if diagnosingOther {
		t.Fatal("doctorHome(real daemon, explicit -config) diagnosingOther = true, want false")
	}
}
