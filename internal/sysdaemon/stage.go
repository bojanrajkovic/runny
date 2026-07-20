package sysdaemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bojanrajkovic/runny/internal/home"
)

// KeyCopy is one distinct source key file → its in-home destination.
type KeyCopy struct {
	Src string // resolved absolute source (expanded from private_key_path)
	Dst string // <homeDir>/<basename>, with a -<hash> suffix on basename collision
}

// StagePlan is the fully-resolved staging decision: the config bytes to write
// into the home (source paths rewritten to their in-home destinations) and the
// distinct key files to copy in.
type StagePlan struct {
	Config []byte    // rawConfig with each source path replaced by its Dst
	Keys   []KeyCopy // deduped by resolved source
}

// PlanStage computes the StagePlan without touching the filesystem. For each
// pool's github.private_key_path it expands the path (absolute / relative to
// cwd / ~ -> operatorHome), dedupes by resolved source, assigns
// Dst = homeDir/basename(src) (a stable -<hash> suffix only when two DISTINCT
// sources would collide on basename), and rewrites the RAW config bytes by
// literal substring replacement of each authored path string with its Dst —
// never a YAML remarshal, which would strip the schema modeline and comments.
// A source already resolving inside homeDir is left alone (no copy, no
// rewrite), so re-running install-daemon and authoring in-home paths directly
// are both no-ops.
func PlanStage(rawConfig []byte, cfg *home.Config, homeDir, operatorHome string) (StagePlan, error) {
	// Pass 1: expand every pool's path, deduping by resolved source and
	// splitting already-in-home sources (no copy) from ones needing a copy,
	// grouped by basename so a same-basename collision renames every side —
	// including an already-home file's basename, so a DISTINCT to-be-copied
	// source can never be assigned that same plain name and silently
	// overwrite it.
	var order []string                               // distinct expanded sources needing a copy, in first-seen order
	expandedByPool := make([]string, len(cfg.Pools)) // pool index -> its expanded source, reused in pass 3
	byBasename := map[string][]string{}
	dstBySrc := map[string]string{}
	occupiedBasenames := map[string]bool{} // basenames already spoken for by an already-home file
	seen := map[string]bool{}
	for i, p := range cfg.Pools {
		expanded, err := expandKeyPath(p.GitHub.PrivateKeyPath, operatorHome)
		if err != nil {
			return StagePlan{}, fmt.Errorf("pool %s: private_key_path: %w", p.Name, err)
		}
		expandedByPool[i] = expanded
		if seen[expanded] {
			continue
		}
		seen[expanded] = true
		if filepath.Dir(expanded) == homeDir {
			dstBySrc[expanded] = expanded // already in-home: no copy, no rewrite
			occupiedBasenames[filepath.Base(expanded)] = true
			continue
		}
		base := filepath.Base(expanded)
		byBasename[base] = append(byBasename[base], expanded)
		order = append(order, expanded)
	}

	// Pass 2: assign destinations for the to-be-copied sources — a lone
	// source per basename keeps it plain UNLESS an already-home file already
	// occupies that basename; two or more distinct sources sharing a
	// basename (with or without an occupying already-home file) all get
	// hashed names, so no one source silently wins the collision.
	for base, srcs := range byBasename {
		for _, src := range srcs {
			if len(srcs) == 1 && !occupiedBasenames[base] {
				dstBySrc[src] = filepath.Join(homeDir, base)
			} else {
				dstBySrc[src] = filepath.Join(homeDir, hashedBasename(base, src))
			}
		}
	}
	keys := make([]KeyCopy, 0, len(order))
	for _, src := range order {
		keys = append(keys, KeyCopy{Src: src, Dst: dstBySrc[src]})
	}

	// Pass 3: rewrite the raw bytes, replacing each DISTINCT authored path
	// string with its resolved destination. Deduped by raw string (as
	// authored, before expansion — two pools can author the same relative
	// string from the same effective cwd) and replaced longest-first: a
	// per-pool, arrival-order replacement would let one pool's authored
	// string that happens to be a substring of another's (e.g. "app.pem"
	// inside "keys/app.pem") corrupt the other's path before its own
	// replacement runs. Longest-first guarantees a shorter string is never
	// replaced while it is still embedded, unreplaced, inside a longer one.
	dstByRaw := map[string]string{}
	for i, p := range cfg.Pools {
		raw := p.GitHub.PrivateKeyPath
		if _, ok := dstByRaw[raw]; ok {
			continue
		}
		dstByRaw[raw] = dstBySrc[expandedByPool[i]]
	}
	raws := make([]string, 0, len(dstByRaw))
	for raw := range dstByRaw {
		raws = append(raws, raw)
	}
	sort.Slice(raws, func(a, b int) bool { return len(raws[a]) > len(raws[b]) })
	out := rawConfig
	for _, raw := range raws {
		if dst := dstByRaw[raw]; dst != raw {
			out = bytes.ReplaceAll(out, []byte(raw), []byte(dst))
		}
	}
	return StagePlan{Config: out, Keys: keys}, nil
}

// expandKeyPath resolves an authored private_key_path to an absolute source:
// "~" / "~/..." against operatorHome, an already-absolute path unchanged, and
// anything else relative to the current working directory.
func expandKeyPath(raw, operatorHome string) (string, error) {
	switch {
	case raw == "":
		return "", fmt.Errorf("empty path")
	case raw == "~" || strings.HasPrefix(raw, "~/"):
		if operatorHome == "" {
			return "", fmt.Errorf("cannot expand %q: no operator home", raw)
		}
		return filepath.Join(operatorHome, strings.TrimPrefix(raw, "~")), nil
	case filepath.IsAbs(raw):
		return raw, nil
	default:
		abs, err := filepath.Abs(raw)
		if err != nil {
			return "", fmt.Errorf("resolving %q: %w", raw, err)
		}
		return abs, nil
	}
}

// hashedBasename renames base to "<stem>-<hash8><ext>", where hash8 is a short
// stable hash of src — used only when two distinct sources collide on the same
// basename. Stable across reinstalls (hashes the source path, not a pool name or
// count), so a rerun assigns the same destination.
func hashedBasename(base, src string) string {
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	sum := sha256.Sum256([]byte(src))
	return fmt.Sprintf("%s-%x%s", stem, sum[:4], ext)
}

// stageTestConfigTimeout bounds the staged `-test-config` exec, mirroring
// cmd/runnyd/parsegate.go's testConfigTimeout: side-effect-free on the target
// binary's end (no home, no lock, no network), so 10s is generous headroom —
// not a tight race, but no install step may block forever either.
const stageTestConfigTimeout = 10 * time.Second

// stager runs the stage flow shared by every platform installer: copy each
// planned key into the home, write the resolved config, then validate via
// `runnyd -test-config` before Install bootstraps/starts anything. writeOwned
// is the one platform-specific piece — darwin writes 0600 then chowns (the file
// inherits the home's ACL by creation, exactly as a hand-cp would); Windows
// writes 0600 and does NOT chown, relying instead on the home's own inherited
// grant ACE (see scm_windows.go).
type stager struct {
	runnydPath string
	writeOwned func(ctx context.Context, path string, data []byte) error
	log        func(format string, args ...any)
	testConfig verdictTester
}

// stage writes plan's keys and config.yaml into the home via writeOwned, then
// runs `runnyd -test-config <home>/config.yaml` and parses its JSON verdict —
// the same contract runnyctl's upgrade and edit-config gates use — rather than
// gating on the exit code alone: a warn-tier config exits 0, and a fresh
// operator is never given another chance to see those warnings once install
// has moved on. Warnings are surfaced regardless of status; an error-tier (or
// otherwise unrecognized) verdict fails the stage, leaving the home in place.
func (s *stager) stage(ctx context.Context, plan StagePlan) error {
	for _, k := range plan.Keys {
		data, err := os.ReadFile(k.Src)
		if err != nil {
			return fmt.Errorf("reading %s: %w", k.Src, err)
		}
		if err := s.writeOwned(ctx, k.Dst, data); err != nil {
			return err
		}
	}
	configPath := home.Dir(home.SystemHomeDir).ConfigPath()
	if err := s.writeOwned(ctx, configPath, plan.Config); err != nil {
		return err
	}
	fail := func(err error) error {
		return fmt.Errorf("staged config failed validation (home left in place; fix %s and rerun install-daemon): %w",
			configPath, err)
	}
	cctx, cancel := context.WithTimeout(ctx, stageTestConfigTimeout)
	defer cancel()
	v, err := s.testConfig(cctx, s.runnydPath, configPath)
	if err != nil {
		return fail(err)
	}
	for _, w := range v.Warnings {
		s.log("warning: %s", w.Message)
	}
	// Fail closed on anything but ok/warn, matching decideUpgrade and edit-config's
	// same-contract gates: an unrecognized status must never be treated as ok.
	if v.Status != home.VerdictOK && v.Status != home.VerdictWarn {
		return fail(fmt.Errorf("status %q: %s", v.Status, strings.Join(v.Errors, "; ")))
	}
	s.log("staged config.yaml and %d key file(s) into %s; validated by %s -test-config",
		len(plan.Keys), home.SystemHomeDir, s.runnydPath)
	return nil
}
