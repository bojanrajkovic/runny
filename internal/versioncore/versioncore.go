// Package versioncore is the one definition of "what x.y.z am I, and is it
// behind?" shared by every surface that compares runny versions: runnyctl's
// skew warning, the daemon's "a newer runnyd is available" log line, and the
// cold-restart path. Defining it once means the CLI and the daemon can never
// disagree on what "behind" means — a one-sided tweak to the regex or the
// ordering would otherwise let one warn while the other stays silent.
package versioncore

import (
	"regexp"
	"strconv"
	"strings"
)

// WireProtocolVersion is the runny.v1 wire-contract version, published by the
// daemon in GetStatusResponse.protocol_version and read by runnyctl's skew
// check (cmd/runnyctl/skew.go) — one const, both sides, no lockstep bump.
// Defined here — a stdlib-only leaf both internal/socket and runnyctl can
// import — rather than in internal/socket, so runnyctl need not pull in
// internal/socket's much heavier dependency graph (statemachine, cgo/vz) just
// to read one constant. Bump it when the daemon gains a feature a client must
// detect before relying on it. Version 1 introduced pause/resume command
// acknowledgement (SlotStatus.recent_applied_command_ids): a client confirms a
// pause/resume from the command id only against a daemon advertising >= 1.
// Version 2 introduced reload-convergence confirmation (boot_id,
// config_sha256, drain_seq, exit_held): a reload-following client confirms
// the respawn by boot_id flip + config hash only against a daemon advertising
// >= 2, and otherwise falls back to daemon_started and warns it cannot
// verify.
const WireProtocolVersion uint32 = 2

var coreRE = regexp.MustCompile(`^\d+\.\d+\.\d+`)

// Core returns the leading x.y.z of a version label, or "" if it does not start
// with one. A release daemon and a release CLI both publish a suffixed label
// (0.6.0-beta.<sha>) while sharing the same core, so normalizing both before
// comparing keeps a same-commit pair from false-alarming. Anchored at the start
// (mirroring the build's re.match capture and the app's versionCore), so an
// unstamped "dev" label, or an unexpected prefix, yields "" — quiet rather than
// a triple mis-extracted from the middle.
func Core(s string) string {
	return coreRE.FindString(s)
}

// Compare orders two x.y.z cores numerically: -1 if a < b, 0 if equal, 1 if
// a > b. Inputs must be Core output (^\d+\.\d+\.\d+) so each part parses; an
// empty input (an unstamped build) splits to 0.0.0, so callers that must stay
// quiet on "" guard before calling rather than relying on the ordering.
func Compare(a, b string) int {
	pa, pb := split(a), split(b)
	for i := range pa {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func split(s string) [3]int {
	var p [3]int
	for i, part := range strings.SplitN(s, ".", 3) {
		p[i], _ = strconv.Atoi(part)
	}
	return p
}
