package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDecideUpgrade(t *testing.T) {
	cases := []struct {
		status  string
		force   bool
		proceed bool
	}{
		{verdictOK, false, true},
		{verdictOK, true, true},
		// Warn refuses without --force, proceeds with it.
		{verdictWarn, false, false},
		{verdictWarn, true, true},
		// Error refuses, and --force does NOT override a hard incompatibility.
		{verdictError, false, false},
		{verdictError, true, false},
		// An unexpected/empty status fails closed (never a guessed proceed).
		{"weird", false, false},
		{"", true, false},
	}
	for _, tc := range cases {
		proceed, refusal := decideUpgrade(tc.status, tc.force)
		if proceed != tc.proceed {
			t.Errorf("decideUpgrade(%q, force=%v) proceed=%v, want %v", tc.status, tc.force, proceed, tc.proceed)
		}
		if proceed && refusal != "" {
			t.Errorf("decideUpgrade(%q, force=%v) proceeds but carries refusal %q", tc.status, tc.force, refusal)
		}
		if !proceed && refusal == "" {
			t.Errorf("decideUpgrade(%q, force=%v) refuses with an empty message", tc.status, tc.force)
		}
	}
}

// The Warn refusal must point the operator at --force; the Error refusal must not
// (forcing past a hard error is exactly what's forbidden).
func TestDecideUpgradeRefusalMessages(t *testing.T) {
	if _, r := decideUpgrade(verdictWarn, false); !strings.Contains(r, "--force") {
		t.Errorf("warn refusal should mention --force: %q", r)
	}
	if _, r := decideUpgrade(verdictError, true); strings.Contains(r, "--force") {
		t.Errorf("error refusal must not suggest --force (it can't override a hard error): %q", r)
	}
}

// configVerdict must decode the cross-language contract runnyd -test-config emits —
// a json-tag typo here would silently misread every verdict.
func TestConfigVerdictUnmarshalsContract(t *testing.T) {
	const j = `{"status":"warn","errors":["image-ref: pool mac: bad"],"warnings":[{"kind":"deadline-too-short","message":"too short"}]}`
	var v configVerdict
	if err := json.Unmarshal([]byte(j), &v); err != nil {
		t.Fatal(err)
	}
	if v.Status != verdictWarn || len(v.Errors) != 1 || len(v.Warnings) != 1 {
		t.Fatalf("decoded shape wrong: %+v", v)
	}
	if v.Warnings[0].Kind != "deadline-too-short" || v.Warnings[0].Message != "too short" {
		t.Errorf("warning decode wrong: %+v", v.Warnings[0])
	}
}
