package main

import (
	"testing"

	"github.com/bojanrajkovic/runny/internal/home"
)

// The known #351 bug: an operator's own ~/.runny doesn't own SystemHomeDir,
// so ResolveServer resolves them to their per-user home. Pointing -doctor
// straight at the system config must still diagnose the system home — not
// the invoker's own — since config.yaml always lives at <home>/config.yaml.
func TestDoctorHomeExplicitConfigPinsHome(t *testing.T) {
	invoker := home.Dir("/Users/someone/.runny")
	configFlag := home.SystemHomeDir + "/config.yaml"
	if dir := doctorHome(true, configFlag, invoker); dir != home.Dir(home.SystemHomeDir) {
		t.Fatalf("doctorHome(explicit system -config) dir = %q, want %q", dir, home.SystemHomeDir)
	}
}

// A bare `-doctor` (no -config) keeps whatever ResolveServer already picked
// — that ownership-based fallback already describes an operator's own
// per-user daemon correctly beside an installed system home.
func TestDoctorHomeNoConfigKeepsResolved(t *testing.T) {
	resolved := home.Dir("/Users/someone/.runny")
	if dir := doctorHome(true, "", resolved); dir != resolved {
		t.Fatalf("doctorHome(no -config) dir = %q, want %q", dir, resolved)
	}
}

// Outside doctor mode, -config must never change which home the real daemon
// binds/writes as — it only selects which config.yaml to load.
func TestDoctorHomeRealDaemonUnaffected(t *testing.T) {
	resolved := home.Dir("/Users/someone/.runny")
	if dir := doctorHome(false, home.SystemHomeDir+"/config.yaml", resolved); dir != resolved {
		t.Fatalf("doctorHome(real daemon, explicit -config) dir = %q, want %q", dir, resolved)
	}
}
