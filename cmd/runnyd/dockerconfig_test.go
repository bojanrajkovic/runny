package main

import (
	"testing"

	"github.com/bojanrajkovic/runny/internal/home"
)

// The per-user agent must keep the standard ~/.docker lookup. Its home is a
// real user home, so an operator's existing `docker login` is already
// reachable — overriding DOCKER_CONFIG there would point the daemon at an
// empty dir and silently downgrade every private pull to anonymous.
func TestDockerConfigDefaultLeavesPerUserAgentAlone(t *testing.T) {
	if got := dockerConfigDefault(false, home.Dir("/Users/someone/.runny"), ""); got != "" {
		t.Fatalf("dockerConfigDefault(perUser) = %q, want \"\" (leave ~/.docker alone)", got)
	}
}

// The system daemon runs as a service account whose home is /var/empty, so the
// default ~/.docker/config.json is neither present for it nor writable by the
// operator. It needs the in-home location.
func TestDockerConfigDefaultRedirectsSystemDaemon(t *testing.T) {
	dir := home.Dir(home.SystemHomeDir)
	if got := dockerConfigDefault(true, dir, ""); got != dir.DockerConfigDir() {
		t.Fatalf("dockerConfigDefault(system) = %q, want %q", got, dir.DockerConfigDir())
	}
}

// An operator-set DOCKER_CONFIG always wins, in either deployment.
func TestDockerConfigDefaultNeverOverridesOperator(t *testing.T) {
	for _, sys := range []bool{true, false} {
		if got := dockerConfigDefault(sys, home.Dir(home.SystemHomeDir), "/operator/choice"); got != "" {
			t.Fatalf("dockerConfigDefault(isSystemDaemon=%v, operator-set) = %q, want \"\"", sys, got)
		}
	}
}
