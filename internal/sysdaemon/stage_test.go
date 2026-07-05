package sysdaemon

import (
	"strings"
	"testing"

	"github.com/bojanrajkovic/runny/internal/home"
)

func poolWithKey(name, keyPath string) home.PoolConfig {
	return home.PoolConfig{Name: name, GitHub: home.GitHubConfig{PrivateKeyPath: keyPath}}
}

const testHomeDir = "/Library/Application Support/runny"

func TestPlanStageExpandsAndRewrites(t *testing.T) {
	raw := []byte("private_key_path: /Users/op/keys/runner-app.pem\n")
	cfg := &home.Config{Pools: []home.PoolConfig{poolWithKey("mac", "/Users/op/keys/runner-app.pem")}}
	plan, err := PlanStage(raw, cfg, testHomeDir, "/Users/op")
	if err != nil {
		t.Fatalf("PlanStage: %v", err)
	}
	if len(plan.Keys) != 1 || plan.Keys[0].Src != "/Users/op/keys/runner-app.pem" ||
		plan.Keys[0].Dst != testHomeDir+"/runner-app.pem" {
		t.Fatalf("keys = %+v", plan.Keys)
	}
	if !strings.Contains(string(plan.Config), testHomeDir+"/runner-app.pem") {
		t.Errorf("config not rewritten to the in-home dest:\n%s", plan.Config)
	}
	if strings.Contains(string(plan.Config), "/Users/op/keys/runner-app.pem") {
		t.Errorf("config still carries the source path:\n%s", plan.Config)
	}
}

func TestPlanStageExpandsTilde(t *testing.T) {
	raw := []byte("private_key_path: ~/.runny/runner-app.pem\n")
	cfg := &home.Config{Pools: []home.PoolConfig{poolWithKey("mac", "~/.runny/runner-app.pem")}}
	plan, err := PlanStage(raw, cfg, testHomeDir, "/Users/op")
	if err != nil {
		t.Fatalf("PlanStage: %v", err)
	}
	if len(plan.Keys) != 1 || plan.Keys[0].Src != "/Users/op/.runny/runner-app.pem" {
		t.Fatalf("keys = %+v", plan.Keys)
	}
}

func TestPlanStageDedupesSameSourceAcrossPools(t *testing.T) {
	raw := []byte("private_key_path: /Users/op/app.pem\nprivate_key_path: /Users/op/app.pem\n")
	cfg := &home.Config{Pools: []home.PoolConfig{
		poolWithKey("mac1", "/Users/op/app.pem"),
		poolWithKey("mac2", "/Users/op/app.pem"),
	}}
	plan, err := PlanStage(raw, cfg, testHomeDir, "/Users/op")
	if err != nil {
		t.Fatalf("PlanStage: %v", err)
	}
	if len(plan.Keys) != 1 {
		t.Fatalf("expected one deduped KeyCopy for a shared source, got %+v", plan.Keys)
	}
}

func TestPlanStageHashesDistinctSourcesOnBasenameCollision(t *testing.T) {
	raw := []byte("a: /Users/op/one/app.pem\nb: /Users/op/two/app.pem\n")
	cfg := &home.Config{Pools: []home.PoolConfig{
		poolWithKey("mac1", "/Users/op/one/app.pem"),
		poolWithKey("mac2", "/Users/op/two/app.pem"),
	}}
	plan, err := PlanStage(raw, cfg, testHomeDir, "/Users/op")
	if err != nil {
		t.Fatalf("PlanStage: %v", err)
	}
	if len(plan.Keys) != 2 {
		t.Fatalf("expected two distinct KeyCopy entries, got %+v", plan.Keys)
	}
	if plan.Keys[0].Dst == plan.Keys[1].Dst {
		t.Errorf("colliding basenames must get distinct destinations, both got %q", plan.Keys[0].Dst)
	}
	for _, k := range plan.Keys {
		if k.Dst == testHomeDir+"/app.pem" {
			t.Errorf("a colliding source must not keep the plain basename: %+v", k)
		}
	}
}

func TestPlanStageAlreadyInHomeIsNoOp(t *testing.T) {
	inHome := testHomeDir + "/runner-app.pem"
	raw := []byte("private_key_path: " + inHome + "\n")
	cfg := &home.Config{Pools: []home.PoolConfig{poolWithKey("mac", inHome)}}
	plan, err := PlanStage(raw, cfg, testHomeDir, "/Users/op")
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
	inHome := testHomeDir + "/runner-app.pem"
	raw := []byte("a: " + inHome + "\nb: /Users/op/other/runner-app.pem\n")
	cfg := &home.Config{Pools: []home.PoolConfig{
		poolWithKey("mac1", inHome),
		poolWithKey("mac2", "/Users/op/other/runner-app.pem"),
	}}
	plan, err := PlanStage(raw, cfg, testHomeDir, "/Users/op")
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
	raw := []byte("a: app.pem\nb: keys/app.pem\n")
	cfg := &home.Config{Pools: []home.PoolConfig{
		poolWithKey("mac1", "app.pem"),
		poolWithKey("mac2", "keys/app.pem"),
	}}
	plan, err := PlanStage(raw, cfg, testHomeDir, "/Users/op")
	if err != nil {
		t.Fatalf("PlanStage: %v", err)
	}
	if len(plan.Keys) != 2 {
		t.Fatalf("expected two distinct KeyCopy entries, got %+v", plan.Keys)
	}
	for _, k := range plan.Keys {
		if !strings.HasPrefix(k.Dst, testHomeDir+"/") || strings.Contains(k.Dst, "keys/") {
			t.Errorf("destination must be a flat homeDir path, not a corrupted nested one: %+v", k)
		}
	}
	if strings.Contains(string(plan.Config), "keys/") {
		t.Errorf("pool b's rewritten path must not retain the stale \"keys/\" prefix: %s", plan.Config)
	}
}

func TestPlanStagePreservesModelineAndComments(t *testing.T) {
	raw := []byte("# yaml-language-server: $schema=https://example/schema.json\n" +
		"# a comment mentioning /Users/op/app.pem in passing\n" +
		"private_key_path: /Users/op/app.pem\n")
	cfg := &home.Config{Pools: []home.PoolConfig{poolWithKey("mac", "/Users/op/app.pem")}}
	plan, err := PlanStage(raw, cfg, testHomeDir, "/Users/op")
	if err != nil {
		t.Fatalf("PlanStage: %v", err)
	}
	if !strings.HasPrefix(string(plan.Config), "# yaml-language-server:") {
		t.Errorf("modeline must survive a literal substring rewrite: %s", plan.Config)
	}
	// ponytail: literal substring replace also touches a comment mentioning the
	// same path — an accepted ceiling (see stage.go's PlanStage doc); pinning it
	// here documents the behavior rather than silently drifting.
	if strings.Contains(string(plan.Config), "/Users/op/app.pem") {
		t.Errorf("expected the comment's mention to be rewritten too (documented ceiling): %s", plan.Config)
	}
}
