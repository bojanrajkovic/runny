package home

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestSlugHostname(t *testing.T) {
	cases := map[string]string{
		"junction.local":     "junction",
		"Bojans-MacBook.Pro": "bojans-macbook",
		"weird__host__name":  "weird-host-name",
		"---":                "runny",
		"":                   "runny",
		"UPPER":              "upper",
		"a-very-long-hostname-that-exceeds-the-cap": "a-very-long-hostname-tha",
	}
	for in, want := range cases {
		if got := slugHostname(in); got != want {
			t.Errorf("slugHostname(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestInstancePrefixPersists(t *testing.T) {
	d := Dir(t.TempDir())
	first, err := d.InstancePrefix()
	if err != nil {
		t.Fatalf("InstancePrefix: %v", err)
	}
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9-]*-[0-9a-f]{8}$`).MatchString(first) {
		t.Errorf("prefix %q is not <slug>-<rand8>", first)
	}
	// A second call returns the same value — it's persisted, not regenerated.
	second, err := d.InstancePrefix()
	if err != nil {
		t.Fatalf("InstancePrefix (2nd): %v", err)
	}
	if first != second {
		t.Errorf("prefix not stable: %q then %q", first, second)
	}
	// An empty/corrupt file regenerates rather than returning "".
	if err := os.WriteFile(d.InstanceIDPath(), []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	third, err := d.InstancePrefix()
	if err != nil || third == "" {
		t.Errorf("regenerate from empty file: %q, %v", third, err)
	}
}

func TestValidateRunnerNames(t *testing.T) {
	// Worst-case prefix: 24-char slug + dash + rand8 = 33 chars.
	long := "abcdefghijklmnopqrstuvwx-a1b2c3d4"
	ok := []PoolConfig{{Name: "mac", Count: 2}}
	if err := ValidateRunnerNames(long, ok); err != nil {
		t.Errorf("short pool name should fit: %v", err)
	}
	// 33 + 1 + 21 + 1 + 1 + 1 + 8 = 66 > 64: must be rejected, naming the pool.
	bad := []PoolConfig{{Name: "macos-arm64-productio", Count: 2}}
	err := ValidateRunnerNames(long, bad)
	if err == nil || !strings.Contains(err.Error(), "macos-arm64-productio") {
		t.Errorf("oversized name not rejected: %v", err)
	}
	// A hand-edited oversized instance-id is caught the same way.
	if err := ValidateRunnerNames(strings.Repeat("x", 60), ok); err == nil {
		t.Error("oversized persisted prefix not rejected")
	}
}
