package home

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestOperatorGrantByUIDHasBit pins the unknown-vs-root distinction on disk:
// a nil ByUID (unreadable peer cred) writes no by_uid key at all and reads
// back nil — never a fabricated 0, which would attribute the grant to root —
// while a real uid-0 grantor survives the round-trip as 0.
func TestOperatorGrantByUIDHasBit(t *testing.T) {
	d := Dir(t.TempDir())
	rootUID := uint32(0)
	for _, g := range []OperatorGrant{
		{Action: "grant", TargetUser: "alice"},
		{Action: "grant", ByUID: &rootUID, ByUser: "root", TargetUser: "bob"},
	} {
		if err := d.AppendOperatorGrant(g); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(d.OperatorGrantsPath())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if strings.Contains(lines[0], "by_uid") {
		t.Errorf("unknown grantor wrote a by_uid key: %s", lines[0])
	}
	got, err := d.ReadOperatorGrants()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 records, got %+v", got)
	}
	if got[0].ByUID != nil {
		t.Errorf("unknown grantor read back by_uid=%d, want nil", *got[0].ByUID)
	}
	if got[1].ByUID == nil || *got[1].ByUID != 0 {
		t.Errorf("root grantor's uid 0 lost its has-bit: %+v", got[1])
	}
}

func TestOperatorGrantsRoundTrip(t *testing.T) {
	d := Dir(t.TempDir())
	base := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	uid1, uid2 := uint32(501), uint32(502)
	want := []OperatorGrant{
		{Action: "grant", ByUID: &uid1, ByUser: "brajkovic", TargetUID: 502, TargetUser: "alice", At: base},
		{Action: "grant", ByUID: &uid2, ByUser: "alice", TargetUID: 503, TargetUser: "bob", At: base.Add(time.Hour)},
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
