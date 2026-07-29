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
// It deliberately reports only WHICH home, not whether that home is the
// caller's own. An earlier shape returned a "diagnosing someone else's home"
// flag and the writes were gated on it, which made the read-only contract true
// for -config and false for a bare -doctor — the caller's own home was still
// scaffolded, re-moded and given an instance-id. Whose home it is turns out not
// to be the interesting question: -doctor writes to no home at all, so checkOnly
// alone gates every write and there is nothing left for a second return value
// to decide.
func doctorHome(checkOnly bool, configFlag string, resolved home.Dir) home.Dir {
	if checkOnly && configFlag != "" {
		return home.Dir(filepath.Dir(configFlag))
	}
	return resolved
}
