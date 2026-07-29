package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/bojanrajkovic/runny/internal/home"
)

// seedDiagnosableHome scaffolds a home with a config file in it — the shape a
// doctor pass runs against. Returns the home and its config path.
func seedDiagnosableHome(t *testing.T) (home.Dir, string) {
	t.Helper()
	dir := home.Dir(t.TempDir())
	if err := dir.Ensure(); err != nil {
		t.Fatalf("scaffolding the home: %v", err)
	}
	path := dir.ConfigPath()
	if err := os.WriteFile(path, []byte("pools: []\n"), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return dir, path
}

// snapshotTree records every path under root with its size and mode, so a
// before/after comparison catches a created file, a rewritten one, and a
// re-moded one alike. Content is deliberately not hashed: size+mode is enough
// to catch every write these paths make, and keeps the failure message short.
func snapshotTree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out = append(out, rel+" "+info.Mode().String()+" "+strconv.FormatInt(info.Size(), 10))
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	sort.Strings(out)
	return out
}

// checkRunnerNamespace used to resolve the prefix through the GENERATING
// accessor unconditionally, persisting an instance-id owned by whoever ran the
// command — under `sudo runnyd -doctor -config <system home>`, root. The
// service account could then be unable to read it, and InstancePrefix surfaces
// a read denial as an error rather than regenerating, so runner-namespace
// would fail at every subsequent startup: a read-only diagnostic bricking the
// install it was run to diagnose.
func TestCheckRunnerNamespaceReadOnlyPersistsNothing(t *testing.T) {
	dir, _ := seedDiagnosableHome(t)
	cfg := &home.Config{}

	c := checkRunnerNamespace(dir, cfg, true)

	if _, err := os.Stat(dir.InstanceIDPath()); !os.IsNotExist(err) {
		t.Errorf("a read-only doctor pass created %s (stat err = %v), want no file", dir.InstanceIDPath(), err)
	}
	if !c.OK {
		t.Errorf("runner-namespace on a never-started home = FAIL (%s), want OK on the worst-case prefix", c.Detail)
	}
	if !strings.Contains(c.Detail, home.WorstCasePrefix()) {
		t.Errorf("detail = %q, want it to name the worst-case prefix it validated against", c.Detail)
	}
}

// The startup path is the opposite: generating the id there is deliberate —
// it is also the proof that instance-id is writable, a startup requirement a
// doctor pass must not paper over.
func TestCheckRunnerNamespaceWritableHomeStillPersists(t *testing.T) {
	dir, _ := seedDiagnosableHome(t)

	c := checkRunnerNamespace(dir, &home.Config{}, false)
	if !c.OK {
		t.Fatalf("runner-namespace on a writable home = FAIL (%s), want OK", c.Detail)
	}
	b, err := os.ReadFile(dir.InstanceIDPath())
	if err != nil {
		t.Fatalf("the startup path must persist instance-id: %v", err)
	}
	if strings.TrimSpace(string(b)) != c.Detail {
		t.Errorf("persisted id %q, but the check reported %q", strings.TrimSpace(string(b)), c.Detail)
	}
}

// instance-id is a hostname slug plus four random bytes — not a secret, and it
// sits inside a 0700 home. 0600 predates the artifact-mode loosening and is
// what turns a stray root-created id into a permanent startup failure for the
// service account.
func TestInstanceIDIsGroupAndWorldReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows mode bits are inert: Go reports 0666 for every file")
	}
	dir, _ := seedDiagnosableHome(t)
	if _, err := dir.InstancePrefix(); err != nil {
		t.Fatalf("generating the instance id: %v", err)
	}
	st, err := os.Stat(dir.InstanceIDPath())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Mode().Perm() != 0o644 {
		t.Errorf("instance-id mode = %v, want 0644 (readable inside the 0700 home)", st.Mode().Perm())
	}
}

// The class guard, not a per-write assertion: a read-only doctor pass must
// leave the diagnosed home byte-identical. This is what catches the NEXT
// write added to a check — the failure mode that produced this bug, where the
// diagnosingOther guard was honoured at one site and two others grew past it.
func TestDoctorSuiteWritesNothingToAReadOnlyHome(t *testing.T) {
	dir, configPath := seedDiagnosableHome(t)
	// Built in Go rather than loaded: the suite needs at least one pool (a
	// zero-pool config is invalid), but a resolvable image would make the
	// image-resolve check reach a registry. An unparseable ref fails at
	// oci.ParseRef and skips the network call, leaving every LOCAL check —
	// the ones that could write — running exactly as they do in production.
	cfg := &home.Config{Pools: []home.PoolConfig{{Name: "p", OS: "linux", Count: 1, Image: ""}}}

	before := snapshotTree(t, dir.String())
	// nil clients: the per-client loop is the other network one.
	for _, c := range makeDoctor(dir, configPath, cfg, nil, true)(context.Background()) {
		t.Logf("%-28s ok=%v %s", c.Name, c.OK, c.Detail)
	}
	after := snapshotTree(t, dir.String())

	if len(before) != len(after) {
		t.Fatalf("the doctor pass changed the home's contents:\n before: %v\n  after: %v", before, after)
	}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("the doctor pass wrote to the diagnosed home: %q became %q", before[i], after[i])
		}
	}
}
