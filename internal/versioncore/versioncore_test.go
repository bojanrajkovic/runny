package versioncore

import "testing"

func TestCore(t *testing.T) {
	cases := []struct{ in, want string }{
		{"0.6.0", "0.6.0"},
		// A release pair shares the core; the sha-bearing suffix is dropped so a
		// stamped daemon (0.6.0-beta.<sha>) and a stamped CLI (0.6.0) compare equal.
		{"0.6.0-beta.abc12345", "0.6.0"},
		{"1.20.3+meta", "1.20.3"},
		// Anchored at the start: an unstamped "dev" label, or anything not opening
		// with x.y.z, yields "" → callers stay quiet rather than mis-extract.
		{"dev", ""},
		{"", ""},
		{"v0.6.0", ""},
		{"0.6", ""},
		{"12.34.56-rc.1", "12.34.56"},
		// A triple that isn't the leading token must not be mis-extracted.
		{"ci-2024.01.15-0.6.0", ""},
	}
	for _, tc := range cases {
		if got := Core(tc.in); got != tc.want {
			t.Errorf("Core(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.6.0", "0.6.0", 0},
		{"0.5.0", "0.6.0", -1},
		{"0.6.0", "0.5.0", 1},
		{"0.6.0", "0.6.1", -1},
		{"1.0.0", "0.9.9", 1},
		// Numeric, not lexicographic: 10 > 9.
		{"0.10.0", "0.9.0", 1},
	}
	for _, tc := range cases {
		if got := Compare(tc.a, tc.b); got != tc.want {
			t.Errorf("Compare(%q,%q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
