package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/bojanrajkovic/runny/internal/home"
	"github.com/bojanrajkovic/runny/internal/socket"
)

// privateKeyPermsCheck names the App-key permissions doctor check. It is
// deliberately ADVISORY: a group/world-readable key is a security-hygiene
// footgun, not an operational blocker — the daemon still reads and
// authenticates with it — so failedChecks and splitPreflightChecks exclude it
// from the startup and reload gates. Refusing to boot over it would be strictly
// worse (no daemon at all), and since the exit gate (localConfigChecks) does not
// check perms, gating here would let a mid-drain config edit be green-lit by the
// exit gate and then refused by the respawn child. competing-registration is
// excluded from the gates for the same reasoning. It stays loud via
// `runnyctl doctor`.
const privateKeyPermsCheck = "private-key-perms"

// checkPrivateKeyPerms flags configured private_key_path files that are group-
// or world-accessible. The daemon is meticulous about 0600 on its own artifacts
// but reads whatever App key the operator landed without asserting its mode.
// Local and deterministic — no network, so no checkBudget. Distinct paths are
// checked once even when shared across pools; a Stat error is skipped (an
// unreadable key is already a hard startup failure owned by buildClients).
func checkPrivateKeyPerms(cfg *home.Config) socket.DoctorCheck {
	var loose []string
	seen := map[string]bool{}
	for _, p := range cfg.Pools {
		path := p.GitHub.PrivateKeyPath
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			loose = append(loose, fmt.Sprintf("%s (%#o)", path, perm))
		}
	}
	if len(loose) > 0 {
		return socket.DoctorCheck{Name: privateKeyPermsCheck, OK: false, Detail: fmt.Sprintf(
			"group/world-accessible App key(s) — chmod 600: %s", strings.Join(loose, ", "),
		)}
	}
	return socket.DoctorCheck{Name: privateKeyPermsCheck, OK: true, Detail: "private_key_path file(s) are not group/world-accessible"}
}
