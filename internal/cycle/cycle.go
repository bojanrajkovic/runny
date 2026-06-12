// Package cycle owns the per-cycle artifact record: every teardown — success
// or failure — writes a cycle.json timeline, the machine-readable currency
// behind `runnyctl why` (ADR-0004). Failure cycles additionally retain
// post-mortem files alongside it.
package cycle

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Result classifies a finished cycle.
type Result string

const (
	ResultSuccess Result = "success"
	ResultFailure Result = "failure"
)

// Outcome classifies how a state was left.
type Outcome string

const (
	OutcomeOK       Outcome = "ok"
	OutcomeError    Outcome = "error"
	OutcomeDeadline Outcome = "deadline"
)

// Record is the cycle.json schema. It mirrors runny.v1.CycleRecord; the proto
// is the wire shape, this is the disk shape, and the daemon converts.
type Record struct {
	CycleID string `json:"cycle_id"`
	Slot    string `json:"slot"`
	// Image is the pool's configured ref at cycle time (intent);
	// ImageDigest is what resolved (truth).
	Image       string        `json:"image,omitempty"`
	ImageDigest string        `json:"image_digest,omitempty"`
	Started     time.Time     `json:"started"`
	Finished    time.Time     `json:"finished"`
	Result      Result        `json:"result"`
	States      []StateRecord `json:"states"`
	VM          VMInfo        `json:"vm,omitzero"`
	Job         *JobInfo      `json:"job,omitempty"`
	Failure     *Failure      `json:"failure,omitempty"`
	// Artifacts are file names retained next to cycle.json (failure cycles).
	Artifacts []string `json:"artifacts,omitempty"`
}

type StateRecord struct {
	State   string    `json:"state"`
	Entered time.Time `json:"entered"`
	Left    time.Time `json:"left"`
	Outcome Outcome   `json:"outcome"`
	Error   string    `json:"error,omitempty"`
}

type VMInfo struct {
	MAC string `json:"mac,omitempty"`
	IP  string `json:"ip,omitempty"`
}

type JobInfo struct {
	Name    string    `json:"name"`
	Started time.Time `json:"started"`
}

type Failure struct {
	State string `json:"state"`
	Error string `json:"error"`
}

// NewID mints a short cycle id (8 hex chars), unique enough per slot.
func NewID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Store reads and writes cycle artifact directories for one slot root
// (home.Dir.SlotCyclesDir).
type Store struct {
	// SlotDir is the per-slot cycles directory.
	SlotDir string
}

// Dir returns (and creates) the artifact directory for a cycle:
// <slot>/<RFC3339-started>-<cycleID>/.
func (s Store) Dir(r *Record) (string, error) {
	name := r.Started.UTC().Format("2006-01-02T15-04-05Z") + "-" + r.CycleID
	dir := filepath.Join(s.SlotDir, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating cycle dir: %w", err)
	}
	return dir, nil
}

// Write persists cycle.json into the record's artifact dir, atomically: a
// crash mid-write must not silently erase the cycle from `runnyctl why` —
// the record is the post-mortem.
func (s Store) Write(r *Record) error {
	dir, err := s.Dir(r)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling cycle record: %w", err)
	}
	tmp := filepath.Join(dir, ".cycle.json.tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("writing cycle.json: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, "cycle.json")); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("placing cycle.json: %w", err)
	}
	return nil
}

// Recent returns up to n most-recent records for the slot, newest first.
func (s Store) Recent(n int) ([]*Record, error) {
	entries, err := os.ReadDir(s.SlotDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing cycles: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	// Directory names sort chronologically by construction.
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	if n > 0 && len(names) > n {
		names = names[:n]
	}
	recs := make([]*Record, 0, len(names))
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(s.SlotDir, name, "cycle.json"))
		if err != nil {
			continue // half-written or foreign dir; skip rather than fail Why
		}
		var r Record
		if err := json.Unmarshal(raw, &r); err != nil {
			continue
		}
		recs = append(recs, &r)
	}
	return recs, nil
}

// Prune enforces retention: keep at most keepCount cycles and remove
// anything older than maxAge.
func (s Store) Prune(keepCount int, maxAge time.Duration, now time.Time) error {
	entries, err := os.ReadDir(s.SlotDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for i, name := range names {
		tooMany := keepCount > 0 && i >= keepCount
		tooOld := maxAge > 0 && olderThan(name, now.Add(-maxAge))
		if tooMany || tooOld {
			if err := os.RemoveAll(filepath.Join(s.SlotDir, name)); err != nil {
				return fmt.Errorf("pruning %s: %w", name, err)
			}
		}
	}
	return nil
}

func olderThan(dirName string, cutoff time.Time) bool {
	// Name shape: 2006-01-02T15-04-05Z-<id>; parse the timestamp prefix.
	i := strings.LastIndexByte(dirName, '-')
	if i < 0 {
		return false
	}
	t, err := time.Parse("2006-01-02T15-04-05Z", dirName[:i])
	if err != nil {
		return false
	}
	return t.Before(cutoff)
}
