package cycle

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func record(slot, id string, started time.Time, result Result) *Record {
	return &Record{
		CycleID: id,
		Slot:    slot,
		Started: started,
		Result:  result,
		States: []StateRecord{
			{State: "CLONE", Entered: started, Left: started.Add(time.Millisecond), Outcome: OutcomeOK},
		},
	}
}

func TestWriteAndRecent(t *testing.T) {
	s := Store{SlotDir: filepath.Join(t.TempDir(), "runner-1")}
	base := time.Date(2026, 6, 9, 22, 0, 0, 0, time.UTC)
	for i, id := range []string{"aaaa0001", "aaaa0002", "aaaa0003"} {
		if err := s.Write(record("runner-1", id, base.Add(time.Duration(i)*time.Minute), ResultSuccess)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	recs, err := s.Recent(2)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2", len(recs))
	}
	if recs[0].CycleID != "aaaa0003" || recs[1].CycleID != "aaaa0002" {
		t.Errorf("wrong order: %s, %s", recs[0].CycleID, recs[1].CycleID)
	}
}

func TestRecentEmptySlot(t *testing.T) {
	s := Store{SlotDir: filepath.Join(t.TempDir(), "never-ran")}
	recs, err := s.Recent(5)
	if err != nil || recs != nil {
		t.Errorf("want nil, nil for missing dir; got %v, %v", recs, err)
	}
}

func TestPruneByCount(t *testing.T) {
	s := Store{SlotDir: filepath.Join(t.TempDir(), "runner-1")}
	base := time.Date(2026, 6, 9, 22, 0, 0, 0, time.UTC)
	for i := range 5 {
		_ = s.Write(record("runner-1", NewID(), base.Add(time.Duration(i)*time.Minute), ResultFailure))
	}
	if err := s.Prune(2, 0, base.Add(time.Hour)); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	recs, _ := s.Recent(0)
	if len(recs) != 2 {
		t.Errorf("after prune: %d records, want 2", len(recs))
	}
}

func TestPruneByAge(t *testing.T) {
	s := Store{SlotDir: filepath.Join(t.TempDir(), "runner-1")}
	old := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	fresh := time.Date(2026, 6, 9, 0, 0, 0, 0, time.UTC)
	_ = s.Write(record("runner-1", "old00001", old, ResultFailure))
	_ = s.Write(record("runner-1", "fresh001", fresh, ResultFailure))
	if err := s.Prune(0, 7*24*time.Hour, fresh.Add(time.Hour)); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	recs, _ := s.Recent(0)
	if len(recs) != 1 || recs[0].CycleID != "fresh001" {
		t.Errorf("age prune kept wrong set: %+v", recs)
	}
}

func TestInjectedKeysRoundTrip(t *testing.T) {
	s := Store{SlotDir: filepath.Join(t.TempDir(), "runner-1")}
	base := time.Date(2026, 6, 9, 22, 0, 0, 0, time.UTC)
	rec := record("runner-1", "key00001", base, ResultFailure)
	rec.Job = &JobInfo{Name: "build", Started: base, OperatorKeys: []string{"SHA256:abc"}}
	rec.InjectedKeys = []InjectedKey{
		{Fingerprint: "SHA256:abc", Comment: "op@host", Injected: base, Reason: "wedged", Outcome: "armed", State: "JOB"},
	}
	if err := s.Write(rec); err != nil {
		t.Fatalf("Write: %v", err)
	}
	recs, err := s.Recent(1)
	if err != nil || len(recs) != 1 {
		t.Fatalf("Recent: %v, %d", err, len(recs))
	}
	got := recs[0]
	if len(got.InjectedKeys) != 1 || got.InjectedKeys[0].Outcome != "armed" || got.InjectedKeys[0].State != "JOB" {
		t.Errorf("injected keys did not round-trip: %+v", got.InjectedKeys)
	}
	if got.Job == nil || len(got.Job.OperatorKeys) != 1 || got.Job.OperatorKeys[0] != "SHA256:abc" {
		t.Errorf("operator keys did not round-trip: %+v", got.Job)
	}
}

func TestOldCycleJSONLoads(t *testing.T) {
	// A cycle.json written before issue #39 (no injected_keys/operator_keys)
	// must unmarshal unchanged.
	s := Store{SlotDir: filepath.Join(t.TempDir(), "runner-1")}
	dir, err := s.Dir(record("runner-1", "old00001", time.Date(2026, 6, 9, 22, 0, 0, 0, time.UTC), ResultSuccess))
	if err != nil {
		t.Fatal(err)
	}
	const old = `{"cycle_id":"old00001","slot":"runner-1","result":"success","states":[],"job":{"name":"build","started":"2026-06-09T22:00:00Z"}}`
	if err := os.WriteFile(filepath.Join(dir, "cycle.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	recs, err := s.Recent(1)
	if err != nil || len(recs) != 1 {
		t.Fatalf("Recent: %v, %d", err, len(recs))
	}
	if recs[0].Job == nil || recs[0].Job.OperatorKeys != nil || recs[0].InjectedKeys != nil {
		t.Errorf("old JSON did not load cleanly: %+v", recs[0])
	}
}

func TestWriteArtifactAtomic(t *testing.T) {
	s := Store{SlotDir: filepath.Join(t.TempDir(), "runner-1")}
	rec := record("runner-1", "art00001", time.Date(2026, 6, 9, 22, 0, 0, 0, time.UTC), ResultFailure)
	if err := s.WriteArtifact(rec, OperatorAccessFile, []byte(`[{"fingerprint":"SHA256:x"}]`)); err != nil {
		t.Fatalf("WriteArtifact: %v", err)
	}
	dir, _ := s.Dir(rec)
	if _, err := os.Stat(filepath.Join(dir, OperatorAccessFile)); err != nil {
		t.Errorf("artifact not placed: %v", err)
	}
	// No leftover tmp file.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("tmp file left behind: %s", e.Name())
		}
	}
	// Artifact name registered exactly once, even on a second write.
	if err := s.WriteArtifact(rec, OperatorAccessFile, []byte(`[{"fingerprint":"SHA256:y"}]`)); err != nil {
		t.Fatalf("WriteArtifact 2: %v", err)
	}
	count := 0
	for _, a := range rec.Artifacts {
		if a == OperatorAccessFile {
			count++
		}
	}
	if count != 1 {
		t.Errorf("artifact registered %d times, want 1", count)
	}
}

func TestRecentSynthesizesOrphanedSidecar(t *testing.T) {
	s := Store{SlotDir: filepath.Join(t.TempDir(), "runner-1")}
	base := time.Date(2026, 6, 9, 22, 0, 0, 0, time.UTC)
	// A real cycle.json record, plus an orphaned dir with only the sidecar.
	if err := s.Write(record("runner-1", "real0001", base, ResultSuccess)); err != nil {
		t.Fatal(err)
	}
	orphan := record("runner-1", "orph0002", base.Add(time.Minute), ResultFailure)
	keys := []InjectedKey{{Fingerprint: "SHA256:abc", Injected: base.Add(time.Minute), Outcome: "armed", State: "JOB"}}
	data, _ := json.Marshal(keys)
	if err := s.WriteArtifact(orphan, OperatorAccessFile, data); err != nil {
		t.Fatal(err)
	}
	// Remove the cycle.json the orphan dir would have (WriteArtifact created
	// only the dir + sidecar; no cycle.json was written for the orphan).
	recs, err := s.Recent(0)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2 (real + synthesized orphan)", len(recs))
	}
	// Newest first: orphan (later timestamp) leads.
	stub := recs[0]
	if stub.CycleID != "orph0002" || stub.Result != ResultFailure {
		t.Errorf("orphan stub wrong: %+v", stub)
	}
	if stub.Failure == nil || stub.Failure.State != "?" {
		t.Errorf("orphan stub failure = %+v", stub.Failure)
	}
	if len(stub.InjectedKeys) != 1 || stub.InjectedKeys[0].Fingerprint != "SHA256:abc" {
		t.Errorf("orphan stub keys = %+v", stub.InjectedKeys)
	}
}

func TestPruneAgesOrphanedSidecar(t *testing.T) {
	// The §9 bound: Recent surfaces an orphaned sidecar, but Prune still ages
	// it under normal retention (an interrupted attempt is not kept forever).
	s := Store{SlotDir: filepath.Join(t.TempDir(), "runner-1")}
	old := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	fresh := time.Date(2026, 6, 9, 0, 0, 0, 0, time.UTC)
	orphan := record("runner-1", "old00001", old, ResultFailure)
	data, _ := json.Marshal([]InjectedKey{{Fingerprint: "SHA256:x", Injected: old, Outcome: "pending", State: "LISTENING"}})
	if err := s.WriteArtifact(orphan, OperatorAccessFile, data); err != nil {
		t.Fatal(err)
	}
	if err := s.Prune(0, 7*24*time.Hour, fresh); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	recs, _ := s.Recent(0)
	if len(recs) != 0 {
		t.Errorf("aged orphan still present: %+v", recs)
	}
}

func TestNewIDShape(t *testing.T) {
	id := NewID()
	if len(id) != 8 {
		t.Errorf("NewID() = %q, want 8 hex chars", id)
	}
	if id == NewID() {
		t.Error("two NewID() calls returned the same value")
	}
}
