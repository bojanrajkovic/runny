package cycle

import (
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

func TestNewIDShape(t *testing.T) {
	id := NewID()
	if len(id) != 8 {
		t.Errorf("NewID() = %q, want 8 hex chars", id)
	}
	if id == NewID() {
		t.Error("two NewID() calls returned the same value")
	}
}
