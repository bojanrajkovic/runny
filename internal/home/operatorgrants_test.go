package home

import (
	"os"
	"testing"
	"time"
)

func TestOperatorGrantsRoundTrip(t *testing.T) {
	d := Dir(t.TempDir())
	base := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	want := []OperatorGrant{
		{Action: "grant", ByUID: 501, ByUser: "brajkovic", TargetUID: 502, TargetUser: "alice", At: base},
		{Action: "grant", ByUID: 502, ByUser: "alice", TargetUID: 503, TargetUser: "bob", At: base.Add(time.Hour)},
	}
	for _, g := range want {
		if err := d.AppendOperatorGrant(g); err != nil {
			t.Fatalf("AppendOperatorGrant: %v", err)
		}
	}
	got, err := d.ReadOperatorGrants()
	if err != nil {
		t.Fatalf("ReadOperatorGrants: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 records, got %d: %+v", len(got), got)
	}
	if got[0].TargetUser != "alice" || got[1].TargetUser != "bob" {
		t.Errorf("records out of order or mangled: %+v", got)
	}
	if !got[1].At.Equal(want[1].At) {
		t.Errorf("At did not round-trip: got %v, want %v", got[1].At, want[1].At)
	}
}

// TestReadOperatorGrantsMissingFile pins the "no grants/revokes yet" case: a
// fresh install has no operator-grants.jsonl at all, which must read as an
// empty list, not an error.
func TestReadOperatorGrantsMissingFile(t *testing.T) {
	d := Dir(t.TempDir())
	got, err := d.ReadOperatorGrants()
	if err != nil {
		t.Fatalf("ReadOperatorGrants: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no records, got %+v", got)
	}
}

// TestReadOperatorGrantsSkipsCorruptLines pins the good-faith-aid contract:
// a corrupt line (the file is operator-writable, per docs/security.md) must
// not break reading the rest of the log.
func TestReadOperatorGrantsSkipsCorruptLines(t *testing.T) {
	d := Dir(t.TempDir())
	if err := d.AppendOperatorGrant(OperatorGrant{Action: "grant", TargetUser: "alice"}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(d.OperatorGrantsPath(), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("not json\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := d.AppendOperatorGrant(OperatorGrant{Action: "grant", TargetUser: "bob"}); err != nil {
		t.Fatal(err)
	}
	got, err := d.ReadOperatorGrants()
	if err != nil {
		t.Fatalf("ReadOperatorGrants: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 valid records (corrupt line skipped), got %d: %+v", len(got), got)
	}
}

// TestReadOperatorGrantsCapsFileSize pins the code-review fix: the file is
// operator-writable and only a good-faith aid (docs/security.md), so an
// accidentally or deliberately grown log must not let a read-only
// ListOperators call allocate unbounded memory. Shrinks the cap (rather
// than writing megabytes of fixture data) and checks the tail — the most
// recent records, which is what latestGrant needs — survives while the
// oldest are dropped.
func TestReadOperatorGrantsCapsFileSize(t *testing.T) {
	orig := operatorGrantsReadCap
	operatorGrantsReadCap = 200
	t.Cleanup(func() { operatorGrantsReadCap = orig })

	d := Dir(t.TempDir())
	for i := range 50 {
		g := OperatorGrant{Action: "grant", TargetUser: "padding", TargetUID: uint32(i)}
		if err := d.AppendOperatorGrant(g); err != nil {
			t.Fatal(err)
		}
	}
	last := OperatorGrant{Action: "revoke", TargetUser: "most-recent"}
	if err := d.AppendOperatorGrant(last); err != nil {
		t.Fatal(err)
	}

	got, err := d.ReadOperatorGrants()
	if err != nil {
		t.Fatalf("ReadOperatorGrants: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no grants parsed")
	}
	if len(got) >= 51 {
		t.Errorf("got %d records, want fewer than the 51 written — the cap did not truncate", len(got))
	}
	if tail := got[len(got)-1]; tail.TargetUser != "most-recent" {
		t.Errorf("last record = %+v, want the most-recent record to survive the cap", tail)
	}
}
