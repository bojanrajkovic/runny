package main

import "github.com/bojanrajkovic/runny/internal/home"

// dockerConfigDefault returns the DOCKER_CONFIG directory runnyd should install
// for itself, or "" to leave registry-credential lookup alone.
//
// Only the system daemon gets redirected. It runs as a service account whose
// home is /var/empty, so the standard ~/.docker/config.json is neither present
// for it nor writable by the operator, and a private pull could never
// authenticate. A per-user agent's home IS the operator's, so its existing
// `docker login` is already on the standard path — redirecting it there would
// point the daemon at an empty directory and silently downgrade every private
// pull to anonymous. An operator-set DOCKER_CONFIG always wins.
func dockerConfigDefault(isSystemDaemon bool, dir home.Dir, current string) string {
	if current != "" || !isSystemDaemon {
		return ""
	}
	return dir.DockerConfigDir()
}
