package home

import (
	"os"
	"regexp"
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
