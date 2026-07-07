package home

import (
	"encoding/json"
	"testing"
)

// Verdict must decode the cross-language contract runnyd -test-config emits —
// a json-tag typo here would silently misread every verdict.
func TestVerdictUnmarshalsContract(t *testing.T) {
	const j = `{"status":"warn","errors":["image-ref: pool mac: bad"],"warnings":[{"kind":"deadline-too-short","message":"too short"}]}`
	var v Verdict
	if err := json.Unmarshal([]byte(j), &v); err != nil {
		t.Fatal(err)
	}
	if v.Status != VerdictWarn || len(v.Errors) != 1 || len(v.Warnings) != 1 {
		t.Fatalf("decoded shape wrong: %+v", v)
	}
	if v.Warnings[0].Kind != "deadline-too-short" || v.Warnings[0].Message != "too short" {
		t.Errorf("warning decode wrong: %+v", v.Warnings[0])
	}
}
