package main

import (
	"path/filepath"

	"github.com/bojanrajkovic/runny/internal/home"
)

// doctorHome resolves the home runnyd -doctor diagnoses. -doctor is
// read-only and must describe whichever deployment the operator named:
// config.yaml always lives at <home>/config.yaml (Dir.ConfigPath), so an
// explicit -config pins dir to its own parent directory instead of
// resolved's ownership-based pick. Without this, every dir-dependent check
// (registry credentials, image-cache annotation, disk headroom) kept
// describing ResolveServer's pick even when an operator pointed -doctor
// straight at another deployment's config — most often SystemHomeDir's, to
// diagnose the system daemon (#351).
//
// Only doctor mode is affected: the real daemon's own dir resolution must
// never change — there, -config only selects which config.yaml to load,
// never which home it binds/writes as. A bare `-doctor` (no -config) keeps
// resolved as-is, which already falls back to an operator's own ~/.runny
// when they don't own an installed system home.
//
// diagnosingOther reports whether dir was pinned to a named deployment that
// may not be the caller's own — the caller must skip any write (Dir.Ensure,
// namely) when true: -doctor is documented read-only, and MkdirAll+Chmod'ing
// another deployment's home (or a permission error attempting to, for an
// operator without chmod rights on it) would break that promise the moment
// -config points elsewhere.
func doctorHome(checkOnly bool, configFlag string, resolved home.Dir) (dir home.Dir, diagnosingOther bool) {
	if checkOnly && configFlag != "" {
		return home.Dir(filepath.Dir(configFlag)), true
	}
	return resolved, false
}
