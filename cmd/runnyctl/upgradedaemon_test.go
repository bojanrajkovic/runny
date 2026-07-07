package main

import (
	"strings"
	"testing"

	"github.com/bojanrajkovic/runny/internal/home"
)

func TestDecideUpgrade(t *testing.T) {
	cases := []struct {
		status  string
		force   bool
		proceed bool
	}{
		{home.VerdictOK, false, true},
		{home.VerdictOK, true, true},
		// Warn refuses without --force, proceeds with it.
		{home.VerdictWarn, false, false},
		{home.VerdictWarn, true, true},
		// Error refuses, and --force does NOT override a hard incompatibility.
		{home.VerdictError, false, false},
		{home.VerdictError, true, false},
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
	if _, r := decideUpgrade(home.VerdictWarn, false); !strings.Contains(r, "--force") {
		t.Errorf("warn refusal should mention --force: %q", r)
	}
	if _, r := decideUpgrade(home.VerdictError, true); strings.Contains(r, "--force") {
		t.Errorf("error refusal must not suggest --force (it can't override a hard error): %q", r)
	}
}
