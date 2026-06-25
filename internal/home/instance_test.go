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

func TestReadInstancePrefix(t *testing.T) {
	d := Dir(t.TempDir())
	// Absent → ("", false), and read-only: it must not create instance-id.
	if p, ok := d.ReadInstancePrefix(); ok || p != "" {
		t.Errorf("absent instance-id: got (%q, %v), want (\"\", false)", p, ok)
	}
	if _, err := os.Stat(d.InstanceIDPath()); !os.IsNotExist(err) {
		t.Error("ReadInstancePrefix must not create instance-id")
	}
	// Present → the trimmed persisted value.
	if err := os.WriteFile(d.InstanceIDPath(), []byte("myhost-deadbeef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if p, ok := d.ReadInstancePrefix(); !ok || p != "myhost-deadbeef" {
		t.Errorf("persisted instance-id: got (%q, %v), want (\"myhost-deadbeef\", true)", p, ok)
	}
}

func TestWorstCasePrefixBoundsDerivedPrefixes(t *testing.T) {
	w := WorstCasePrefix()
	// Exactly the longest a hostname-derived prefix can be: maxSlug + "-" + tail.
	if want := maxHostnameSlug + 1 + instanceTailLen; len(w) != want {
		t.Errorf("WorstCasePrefix len = %d, want %d", len(w), want)
	}
	// No hostname-derived prefix can exceed it — slugHostname caps the slug — so the
	// stateless gate validating against it can never under-estimate the real prefix.
	for _, h := range []string{"a-very-long-hostname-that-exceeds-the-cap", "mac", "build-server-01"} {
		derived := slugHostname(h) + "-" + strings.Repeat("0", instanceTailLen)
		if len(derived) > len(w) {
			t.Errorf("derived prefix for %q (%d) exceeds worst-case (%d)", h, len(derived), len(w))
		}
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
