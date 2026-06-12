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
	"slices"
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
	Image       string `json:"image,omitempty"`
	ImageDigest string `json:"image_digest,omitempty"`
	// RunnerVersion is the asset filename of the actions-runner tarball ensured
	// this cycle (e.g. "actions-runner-osx-arm64-2.320.0.tar.gz"); empty when
	// the runner tarball step was skipped (no Runner configured) or the cycle
	// failed before ENSURE_IMAGE completed.
	RunnerVersion string        `json:"runner_version,omitempty"`
	Started       time.Time     `json:"started"`
	Finished      time.Time     `json:"finished"`
	Result        Result        `json:"result"`
	States        []StateRecord `json:"states"`
	VM            VMInfo        `json:"vm,omitzero"`
	Job           *JobInfo      `json:"job,omitempty"`
	Failure       *Failure      `json:"failure,omitempty"`
	// Artifacts are file names retained next to cycle.json (failure cycles).
	Artifacts []string `json:"artifacts,omitempty"`
	// InjectedKeys is the operator debug-key audit trail for this cycle: one
	// entry per attempt (issue #39), including failed and refused ones.
	InjectedKeys []InjectedKey `json:"injected_keys,omitempty"`
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
	// OperatorKeys are SHA256:… fingerprints of operator debug keys present
	// in — or ambiguously attempted against — the guest while this job ran
	// (issue #39). Reading the job record alone answers "did this job run
	// with an operator credential installed?".
	OperatorKeys []string `json:"operator_keys,omitempty"`
}

type Failure struct {
	State string `json:"state"`
	Error string `json:"error"`
}

// InjectedKey records one operator debug-key attempt against a cycle's guest
// (issue #39). The disk shape mirrors runny.v1.InjectedKey.
type InjectedKey struct {
	Fingerprint string    `json:"fingerprint"`
	Comment     string    `json:"comment,omitempty"`
	Injected    time.Time `json:"injected"`
	Reason      string    `json:"reason,omitempty"`
	// Outcome: pending|ok|armed|re-armed|refused|unreachable|error|disarmed.
	Outcome string `json:"outcome"`
	Error   string `json:"error,omitempty"`
	// State at the attempt: LISTENING|JOB|DEBUG.
	State string `json:"state"`
}

// OperatorAccessFile is the write-ahead audit sidecar's name in a cycle dir
// (issue #39): it carries the InjectedKeys before cycle.json lands, so a
// daemon crash mid-attempt does not erase the evidence that an operator
// credential was about to enter (or did enter) a guest.
const OperatorAccessFile = "operator-access.json"

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

// WriteArtifact atomically writes one named artifact into the record's cycle
// dir (tmp-rename, like Write) and ensures it is listed in r.Artifacts. Used
// WRITE-AHEAD for operator-access.json (issue #39): cycle.json lands only at
// finishCycle, and a daemon crash must not erase even the INTENT that an
// operator credential was about to enter a guest.
func (s Store) WriteArtifact(r *Record, name string, data []byte) error {
	dir, err := s.Dir(r)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+name+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", name, err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("placing %s: %w", name, err)
	}
	if !slices.Contains(r.Artifacts, name) {
		r.Artifacts = append(r.Artifacts, name)
	}
	return nil
}

// Recent returns up to n most-recent records for the slot, newest first. A
// cycle dir that holds an operator-access.json but no cycle.json (a daemon
// crash mid-attempt or mid-hold, issue #39) is surfaced as a synthesized stub
// record so the orphaned credential evidence appears in `runnyctl why`
// instead of only sitting on disk until retention deletes it unseen.
// liveCycleID is the slot's currently-running cycle (empty if none): its
// cycle.json is not written until the cycle ends, so without this it would be
// synthesized as a phantom orphan-failure on every healthy in-progress cycle.
func (s Store) Recent(n int, liveCycleID string) ([]*Record, error) {
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
		dir := filepath.Join(s.SlotDir, name)
		raw, err := os.ReadFile(filepath.Join(dir, "cycle.json"))
		if err != nil {
			// A live cycle has no cycle.json yet (it lands at finishCycle) but
			// may already carry a write-ahead sidecar; that is an in-progress
			// cycle, not a crash-orphan, so never synthesize a failure for it.
			if stub := s.synthesizeOrphan(dir); stub != nil && stub.CycleID != liveCycleID {
				recs = append(recs, stub)
			}
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

// synthesizeOrphan builds a stub Record from a cycle dir that has an
// operator-access.json but no readable cycle.json (issue #39). It returns nil
// when the sidecar is absent or unparseable: only credential evidence is worth
// surfacing this way, and a corrupt sidecar is not actionable.
func (s Store) synthesizeOrphan(dir string) *Record {
	raw, err := os.ReadFile(filepath.Join(dir, OperatorAccessFile))
	if err != nil {
		return nil
	}
	var keys []InjectedKey
	if err := json.Unmarshal(raw, &keys); err != nil || len(keys) == 0 {
		return nil
	}
	// The dir name is <RFC3339-started>-<cycleID>; recover both for the stub.
	base := filepath.Base(dir)
	cycleID := base
	if i := strings.LastIndexByte(base, '-'); i >= 0 {
		cycleID = base[i+1:]
	}
	started := keys[0].Injected
	return &Record{
		CycleID:      cycleID,
		Slot:         filepath.Base(s.SlotDir),
		Started:      started,
		Finished:     started,
		Result:       ResultFailure,
		Failure:      &Failure{State: "?", Error: "daemon died with operator credential evidence on disk"},
		InjectedKeys: keys,
		Artifacts:    []string{OperatorAccessFile},
	}
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
