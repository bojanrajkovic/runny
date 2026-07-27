package sysdaemon

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/bojanrajkovic/runny/internal/home"
)

func poolWithKey(name, keyPath string) home.PoolConfig {
	return home.PoolConfig{Name: name, GitHub: home.GitHubConfig{PrivateKeyPath: keyPath}}
}

// stageFixture returns a daemon home and an operator home as NATIVE absolute
// paths.
//
// A unix literal such as "/Library/Application Support/runny" is drive-relative
// on Windows, and PlanStage absolutises a key's source while taking homeDir as
// the caller gave it — so the source gains a volume the home lacks, the
// already-in-home comparison stops matching, and the plan copies a key it should
// have left alone. That is a property of the fixture, not of PlanStage: a real
// caller passes home.Dir, which is volume-qualified on Windows. Building both
// paths with filepath keeps these tests about staging logic on every platform.
func stageFixture(t *testing.T) (homeDir, opHome string) {
	t.Helper()
	root := t.TempDir()
	return filepath.Join(root, "runny"), filepath.Join(root, "op")
}

func TestPlanStageExpandsAndRewrites(t *testing.T) {
	homeDir, opHome := stageFixture(t)
	src := filepath.Join(opHome, "keys", "runner-app.pem")
	dst := filepath.Join(homeDir, "runner-app.pem")

	raw := []byte("private_key_path: " + src + "\n")
	cfg := &home.Config{Pools: []home.PoolConfig{poolWithKey("mac", src)}}
	plan, err := PlanStage(raw, cfg, homeDir, opHome)
	if err != nil {
		t.Fatalf("PlanStage: %v", err)
	}
	if len(plan.Keys) != 1 || plan.Keys[0].Src != src || plan.Keys[0].Dst != dst {
		t.Fatalf("keys = %+v", plan.Keys)
	}
	if !strings.Contains(string(plan.Config), dst) {
		t.Errorf("config not rewritten to the in-home dest:\n%s", plan.Config)
	}
	if strings.Contains(string(plan.Config), src) {
		t.Errorf("config still carries the source path:\n%s", plan.Config)
	}
}

func TestPlanStageExpandsTilde(t *testing.T) {
	homeDir, opHome := stageFixture(t)
	raw := []byte("private_key_path: ~/.runny/runner-app.pem\n")
	cfg := &home.Config{Pools: []home.PoolConfig{poolWithKey("mac", "~/.runny/runner-app.pem")}}
	plan, err := PlanStage(raw, cfg, homeDir, opHome)
	if err != nil {
		t.Fatalf("PlanStage: %v", err)
	}
	want := filepath.Join(opHome, ".runny", "runner-app.pem")
	if len(plan.Keys) != 1 || plan.Keys[0].Src != want {
		t.Fatalf("keys = %+v, want Src %q", plan.Keys, want)
	}
}

func TestPlanStageDedupesSameSourceAcrossPools(t *testing.T) {
	homeDir, opHome := stageFixture(t)
	src := filepath.Join(opHome, "app.pem")

	raw := []byte("private_key_path: " + src + "\nprivate_key_path: " + src + "\n")
	cfg := &home.Config{Pools: []home.PoolConfig{
		poolWithKey("mac1", src),
		poolWithKey("mac2", src),
	}}
	plan, err := PlanStage(raw, cfg, homeDir, opHome)
	if err != nil {
		t.Fatalf("PlanStage: %v", err)
	}
	if len(plan.Keys) != 1 {
		t.Fatalf("expected one deduped KeyCopy for a shared source, got %+v", plan.Keys)
	}
}

func TestPlanStageHashesDistinctSourcesOnBasenameCollision(t *testing.T) {
	homeDir, opHome := stageFixture(t)
	one := filepath.Join(opHome, "one", "app.pem")
	two := filepath.Join(opHome, "two", "app.pem")

	raw := []byte("a: " + one + "\nb: " + two + "\n")
	cfg := &home.Config{Pools: []home.PoolConfig{
		poolWithKey("mac1", one),
		poolWithKey("mac2", two),
	}}
	plan, err := PlanStage(raw, cfg, homeDir, opHome)
	if err != nil {
		t.Fatalf("PlanStage: %v", err)
	}
	if len(plan.Keys) != 2 {
		t.Fatalf("expected two distinct KeyCopy entries, got %+v", plan.Keys)
	}
	if plan.Keys[0].Dst == plan.Keys[1].Dst {
		t.Errorf("colliding basenames must get distinct destinations, both got %q", plan.Keys[0].Dst)
	}
	plain := filepath.Join(homeDir, "app.pem")
	for _, k := range plan.Keys {
		if k.Dst == plain {
			t.Errorf("a colliding source must not keep the plain basename: %+v", k)
		}
	}
}

func TestPlanStageAlreadyInHomeIsNoOp(t *testing.T) {
	homeDir, opHome := stageFixture(t)
	inHome := filepath.Join(homeDir, "runner-app.pem")

	raw := []byte("private_key_path: " + inHome + "\n")
	cfg := &home.Config{Pools: []home.PoolConfig{poolWithKey("mac", inHome)}}
	plan, err := PlanStage(raw, cfg, homeDir, opHome)
	if err != nil {
		t.Fatalf("PlanStage: %v", err)
	}
	if len(plan.Keys) != 0 {
		t.Errorf("an already-in-home key must not be copied, got %+v", plan.Keys)
	}
	if string(plan.Config) != string(raw) {
		t.Errorf("an already-in-home key's config must be left byte-identical, got %s", plan.Config)
	}
}

// A to-be-copied source must never be assigned the plain basename of a
// DIFFERENT, already-in-home key — that would have stage() overwrite the
// already-home file's contents with the new one's, silently swapping which
// App key a pool that authored the already-home path actually gets.
func TestPlanStageDoesNotCollideWithAlreadyHomeBasename(t *testing.T) {
	homeDir, opHome := stageFixture(t)
	inHome := filepath.Join(homeDir, "runner-app.pem")
	other := filepath.Join(opHome, "other", "runner-app.pem")

	raw := []byte("a: " + inHome + "\nb: " + other + "\n")
	cfg := &home.Config{Pools: []home.PoolConfig{
		poolWithKey("mac1", inHome),
		poolWithKey("mac2", other),
	}}
	plan, err := PlanStage(raw, cfg, homeDir, opHome)
	if err != nil {
		t.Fatalf("PlanStage: %v", err)
	}
	if len(plan.Keys) != 1 {
		t.Fatalf("expected exactly one KeyCopy (the already-home source needs none), got %+v", plan.Keys)
	}
	if plan.Keys[0].Dst == inHome {
		t.Fatalf("the distinct source must not be staged onto the already-home file's path: %+v", plan.Keys[0])
	}
}

// A per-pool authored path that is a literal substring of another pool's
// authored path must not corrupt the other pool's rewritten path — a naive
// per-pool bytes.ReplaceAll pass, run in config order, does exactly that.
func TestPlanStageSubstringPathsDoNotCorruptEachOther(t *testing.T) {
	homeDir, opHome := stageFixture(t)
	raw := []byte("a: app.pem\nb: keys/app.pem\n")
	cfg := &home.Config{Pools: []home.PoolConfig{
		poolWithKey("mac1", "app.pem"),
		poolWithKey("mac2", "keys/app.pem"),
	}}
	plan, err := PlanStage(raw, cfg, homeDir, opHome)
	if err != nil {
		t.Fatalf("PlanStage: %v", err)
	}
	if len(plan.Keys) != 2 {
		t.Fatalf("expected two distinct KeyCopy entries, got %+v", plan.Keys)
	}
	prefix := homeDir + string(filepath.Separator)
	for _, k := range plan.Keys {
		if !strings.HasPrefix(k.Dst, prefix) {
			t.Errorf("destination must live directly in homeDir: %+v", k)
			continue
		}
		// Flat, not nested: the leaf must carry no further separator of either
		// flavour, so this reads the same on windows and unix.
		if strings.ContainsAny(strings.TrimPrefix(k.Dst, prefix), `/\`) {
			t.Errorf("destination must be a flat homeDir path, not a corrupted nested one: %+v", k)
		}
	}
	if strings.Contains(string(plan.Config), "keys/") {
		t.Errorf("pool b's rewritten path must not retain the stale \"keys/\" prefix: %s", plan.Config)
	}
}

func TestPlanStagePreservesModelineAndComments(t *testing.T) {
	homeDir, opHome := stageFixture(t)
	src := filepath.Join(opHome, "app.pem")

	raw := []byte("# yaml-language-server: $schema=https://example/schema.json\n" +
		"# a comment mentioning " + src + " in passing\n" +
		"private_key_path: " + src + "\n")
	cfg := &home.Config{Pools: []home.PoolConfig{poolWithKey("mac", src)}}
	plan, err := PlanStage(raw, cfg, homeDir, opHome)
	if err != nil {
		t.Fatalf("PlanStage: %v", err)
	}
	if !strings.HasPrefix(string(plan.Config), "# yaml-language-server:") {
		t.Errorf("modeline must survive a literal substring rewrite: %s", plan.Config)
	}
	// ponytail: literal substring replace also touches a comment mentioning the
	// same path — an accepted ceiling (see stage.go's PlanStage doc); pinning it
	// here documents the behavior rather than silently drifting.
	if strings.Contains(string(plan.Config), src) {
		t.Errorf("expected the comment's mention to be rewritten too (documented ceiling): %s", plan.Config)
	}
}
