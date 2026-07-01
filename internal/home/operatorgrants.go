package home

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// OperatorGrantsPath is the append-only grant/revoke attribution log: the
// home dir's ACL (read via internal/opacl) is the authoritative CURRENT
// operator set, this file is only who-granted-whom-when history, joined in
// at ListOperators time. A good-faith audit aid under the operator-writable
// home, not a tamper-proof control (docs/security.md).
func (d Dir) OperatorGrantsPath() string { return filepath.Join(string(d), "operator-grants.jsonl") }

// OperatorGrant is one append-only entry: an operator granting or revoking
// another. ByUID/ByUser identify the authenticated peer that ran the RPC
// (SO_PEERCRED); TargetUID/TargetUser identify the account granted/revoked.
type OperatorGrant struct {
	Action     string    `json:"action"` // "grant" | "revoke"
	ByUID      uint32    `json:"by_uid"`
	ByUser     string    `json:"by_user"`
	TargetUID  uint32    `json:"target_uid"`
	TargetUser string    `json:"target_user"`
	At         time.Time `json:"at"`
}

// AppendOperatorGrant appends one grant/revoke record.
func (d Dir) AppendOperatorGrant(g OperatorGrant) error {
	data, err := json.Marshal(g)
	if err != nil {
		return fmt.Errorf("marshaling operator grant record: %w", err)
	}
	f, err := os.OpenFile(d.OperatorGrantsPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("opening operator-grants.jsonl: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("appending operator grant record: %w", err)
	}
	return nil
}

// operatorGrantsReadCap bounds ReadOperatorGrants' memory: the file lives
// under the operator-writable home and is a good-faith audit aid, not a
// tamper-proof control, so an accidentally or deliberately grown file must
// not let a read-only ListOperators call allocate unbounded memory. A var,
// not a const, so tests can shrink it instead of writing megabytes of
// fixture data.
var operatorGrantsReadCap int64 = 4 << 20 // 4 MiB, tens of thousands of records

// ReadOperatorGrants parses records in operator-grants.jsonl, oldest first,
// skipping unparseable lines (a corrupt line must not break ListOperators —
// the file is a good-faith aid, not a control). If the file exceeds
// operatorGrantsReadCap, only the last operatorGrantsReadCap bytes are read
// — the most recent records, which is what latestGrant actually needs, not
// the oldest. Returns (nil, nil) when the file does not exist yet (no
// grants/revokes since install).
func (d Dir) ReadOperatorGrants() ([]OperatorGrant, error) {
	f, err := os.Open(d.OperatorGrantsPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("opening operator-grants.jsonl: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("statting operator-grants.jsonl: %w", err)
	}
	if info.Size() > operatorGrantsReadCap {
		if _, err := f.Seek(-operatorGrantsReadCap, io.SeekEnd); err != nil {
			return nil, fmt.Errorf("seeking operator-grants.jsonl: %w", err)
		}
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("reading operator-grants.jsonl: %w", err)
	}
	var grants []OperatorGrant
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var g OperatorGrant
		if err := json.Unmarshal([]byte(line), &g); err != nil {
			continue // includes a partial first line from a mid-record seek
		}
		grants = append(grants, g)
	}
	return grants, nil
}
