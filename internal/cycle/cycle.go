// Package cycle owns the per-cycle artifact record: every teardown — success
// or failure — writes a cycle.json timeline, the machine-readable currency
// behind `runnyctl why`. Failure cycles additionally retain
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
	"strings"
	"time"

	"github.com/bojanrajkovic/runny/internal/home"
)

// Result classifies a finished cycle.
type Result string

const (
	ResultSuccess Result = "success"
	ResultFailure Result = "failure"
)

// Ending classifies *why* a cycle ended — the detail Result's binary
// success/failure can't carry. Derived in runCycle (internal/statemachine) at
// the same point "benign" is computed for backoff accounting; empty (zero
// value) in records written before this field existed.
type Ending string

const (
	EndingSuccess  Ending = "success"
	EndingFailure  Ending = "failure"
	EndingRecycle  Ending = "recycle"  // operator-initiated (runnyctl recycle)
	EndingShutdown Ending = "shutdown" // daemon exit (ctx cancelled)
	EndingWedge    Ending = "wedge"    // teardown could not kill the guest
)

// Outcome classifies how a state was left.
type Outcome string

const (
	OutcomeOK       Outcome = "ok"
	OutcomeError    Outcome = "error"
	OutcomeDeadline Outcome = "deadline"
	// OutcomeWarn: the state did its mandatory job, but a best-effort cleanup
	// sub-step failed (e.g. a teardown destroyed the guest yet could not
	// deregister the runner or delete the clone — a swept-later orphan). Not a
	// failure: it does not escalate the slot's failure streak.
	OutcomeWarn Outcome = "warn"
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
	RunnerVersion string    `json:"runner_version,omitempty"`
	Started       time.Time `json:"started"`
	Finished      time.Time `json:"finished"`
	Result        Result    `json:"result"`
	// Ending is the why behind Result (see the Ending type). Empty in records
	// written before this field existed.
	Ending  Ending        `json:"ending,omitempty"`
	States  []StateRecord `json:"states"`
	VM      VMInfo        `json:"vm,omitzero"`
	Job     *JobInfo      `json:"job,omitempty"`
	Failure *Failure      `json:"failure,omitempty"`
	// Artifacts are file names retained next to cycle.json (failure cycles).
	Artifacts []string `json:"artifacts,omitempty"`
	// InjectedKeys is the operator debug-key audit trail for this cycle: one
	// entry per attempt, including failed and refused ones.
	InjectedKeys []InjectedKey `json:"injected_keys,omitempty"`
	// CycleDir is the absolute path to this cycle's artifact directory. Not
	// persisted to cycle.json (it is always derivable from the directory the
	// file lives in); populated by Store.Recent() for in-memory use and wire
	// serialization.
	CycleDir string `json:"-"`
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
	// The guest's resolved shape as of Boot (see vm.Spec).
	GuestOS     string `json:"guest_os,omitempty"`
	Arch        string `json:"arch,omitempty"`
	CPUCount    uint   `json:"cpu_count,omitempty"`
	MemoryBytes uint64 `json:"memory_bytes,omitempty"`
}

type JobInfo struct {
	Name    string    `json:"name"`
	Started time.Time `json:"started"`
	// OperatorKeys are SHA256:… fingerprints of operator debug keys present
	// in — or ambiguously attempted against — the guest while this job ran.
	// Reading the job record alone answers "did this job run
	// with an operator credential installed?".
	OperatorKeys []string `json:"operator_keys,omitempty"`
}

type Failure struct {
	State string `json:"state"`
	Error string `json:"error"`
}

// InjectedKey records one operator debug-key attempt against a cycle's guest.
// The disk shape mirrors runny.v1.InjectedKey.
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
	// OperatorUID is the kernel-authenticated uid of the peer that ran
	// `runnyctl debug`, read server-side via SO_PEERCRED. nil means "unknown"
	// (a cred-read failure, or a non-numeric identity carried in OperatorSID
	// instead) — distinct from a recorded uid 0, a real possible peer (root
	// bypasses the socket's 0600 mode).
	OperatorUID *uint32 `json:"operator_uid,omitempty"`
	// OperatorUser is the peer identity's username, resolved best-effort at
	// request time; empty if the identity could not be resolved to an account.
	OperatorUser string `json:"operator_user,omitempty"`
	// OperatorSID carries the peer identity when it is not a numeric uid — a
	// Windows SID string, following os/user.User.Uid's platform-native
	// convention. At most one of OperatorUID/OperatorSID is set; both absent
	// means the identity could not be read.
	OperatorSID string `json:"operator_sid,omitempty"`
}

// OperatorAccessFile is the write-ahead audit sidecar's name in a cycle dir:
// it carries the InjectedKeys before cycle.json lands, so a
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
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating cycle dir: %w", err)
	}
	// Re-assert the SLOT dir too. MkdirAll leaves an existing directory's mode
	// alone, so a slot dir created by an older runny stays 0700 -- and a 0700
	// parent blocks traversal to every cycle beneath it however open those are,
	// which would make the artifacts unreachable on exactly the hosts that have
	// history worth reading. Idempotent, so it self-heals on the next cycle.
	if err := os.Chmod(s.SlotDir, 0o755); err != nil {
		return "", fmt.Errorf("setting the mode on %s: %w", s.SlotDir, err)
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
	if err := home.AtomicWrite(filepath.Join(dir, "cycle.json"), data, 0o644); err != nil {
		return err
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	r.CycleDir = dir
	return nil
}

// WriteArtifact atomically writes one named artifact into the record's cycle
// dir (tmp-rename, like Write) and ensures it is listed in r.Artifacts. Used
// WRITE-AHEAD for operator-access.json: cycle.json lands only at
// finishCycle, and a daemon crash must not erase even the INTENT that an
// operator credential was about to enter a guest.
func (s Store) WriteArtifact(r *Record, name string, data []byte) error {
	dir, err := s.Dir(r)
	if err != nil {
		return err
	}
	if err := home.AtomicWrite(filepath.Join(dir, name), data, 0o644); err != nil {
		return err
	}
	if !slices.Contains(r.Artifacts, name) {
		r.Artifacts = append(r.Artifacts, name)
	}
	return nil
}

// Recent returns up to n most-recent records for the slot, newest first. A
// cycle dir that holds an operator-access.json but no cycle.json (a daemon
// crash mid-attempt or mid-hold) is surfaced as a synthesized stub
// record so the orphaned credential evidence appears in `runnyctl why`
// instead of only sitting on disk until retention deletes it unseen.
// liveCycleID is the slot's currently-running cycle (empty if none): its
// cycle.json is not written until the cycle ends, so without this it would be
// synthesized as a phantom orphan-failure on every healthy in-progress cycle.
func (s Store) Recent(n int, liveCycleID string) ([]*Record, error) {
	names, err := s.dirNamesNewestFirst()
	if err != nil {
		return nil, err
	}
	if names == nil { // SlotDir doesn't exist: no cycles have run yet
		return nil, nil
	}
	if n > 0 && len(names) > n {
		names = names[:n]
	}
	recs := make([]*Record, 0, len(names))
	for _, name := range names {
		dir := filepath.Join(s.SlotDir, name)
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
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
		r.CycleDir = dir
		recs = append(recs, &r)
	}
	return recs, nil
}

// synthesizeOrphan builds a stub Record from a cycle dir that has an
// operator-access.json but no readable cycle.json. It returns nil
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
		Ending:       EndingFailure,
		Failure:      &Failure{State: "?", Error: "daemon died with operator credential evidence on disk"},
		InjectedKeys: keys,
		Artifacts:    []string{OperatorAccessFile},
		CycleDir:     dir,
	}
}

// dirNamesNewestFirst lists SlotDir's cycle directories (skipping any
// non-directory entries), newest first. Directory names sort chronologically
// by construction (Store.Dir's RFC3339-prefixed name), so a plain string sort
// reversed is the newest-first order. Returns (nil, nil) when SlotDir doesn't
// exist yet (no cycles have run).
func (s Store) dirNamesNewestFirst() ([]string, error) {
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
	slices.Sort(names)
	slices.Reverse(names)
	return names, nil
}

// Prune enforces retention: keep at most keepCount cycles and remove
// anything older than maxAge.
func (s Store) Prune(keepCount int, maxAge time.Duration, now time.Time) error {
	names, err := s.dirNamesNewestFirst()
	if err != nil {
		return err
	}
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
