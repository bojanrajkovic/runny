package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/types/known/timestamppb"

	runnyv1 "github.com/bojanrajkovic/runny/proto/runny/v1"
)

// A BACKOFF slot must surface how long until it retries — the time-in-state
// column alone doesn't answer the question an operator is actually asking.
func TestRenderStatusShowsBackoffRemaining(t *testing.T) {
	var buf bytes.Buffer
	c := &ctl{out: &buf}
	c.renderStatus(&runnyv1.GetStatusResponse{
		Version:       "test",
		DaemonStarted: timestamppb.New(time.Now().Add(-time.Hour)),
		Slots: []*runnyv1.SlotStatus{
			{
				Slot:           "mac-1",
				State:          runnyv1.SlotState_SLOT_STATE_BACKOFF,
				StateEntered:   timestamppb.New(time.Now().Add(-5 * time.Second)),
				BackoffSeconds: 45,
			},
			{
				Slot:         "mac-2",
				State:        runnyv1.SlotState_SLOT_STATE_LISTENING,
				StateEntered: timestamppb.New(time.Now()),
				RunnerName:   "junction-a1b2c3d4-mac-2-e48657d0",
			},
		},
	})
	out := buf.String()
	if !strings.Contains(out, "retry in") {
		t.Errorf("BACKOFF slot did not show remaining backoff:\n%s", out)
	}
	// A non-BACKOFF slot must not claim a retry countdown.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "mac-2") && strings.Contains(line, "retry in") {
			t.Errorf("LISTENING slot showed a retry countdown: %q", line)
		}
	}
	// A live cycle shows the GitHub-visible runner name; a BACKOFF slot
	// (no runner exists) falls back to the bare slot handle.
	if !strings.Contains(out, "junction-a1b2c3d4-mac-2-e48657d0") {
		t.Errorf("runner name not rendered:\n%s", out)
	}
	if !strings.Contains(out, "mac-1") {
		t.Errorf("BACKOFF slot lost its slot-name fallback:\n%s", out)
	}
}

// A clamped cell must occupy exactly n display columns. The old byte-based
// trunc returned n−1 bytes plus a 3-byte ellipsis displaying as one column,
// so every clamped cell rendered 2 columns narrower than its padded width
// and shifted every column to its right.
func TestTruncDisplayWidth(t *testing.T) {
	cases := []struct {
		s    string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"exactly-ten", 11, "exactly-ten"},
		{"a-much-longer-string", 10, "a-much-lo…"},
		{"ünïcödé-rünés-everywhere", 10, "ünïcödé-r…"},
	}
	for _, tc := range cases {
		got := trunc(tc.s, tc.n)
		if got != tc.want {
			t.Errorf("trunc(%q, %d) = %q, want %q", tc.s, tc.n, got, tc.want)
		}
		if w := utf8.RuneCountInString(got); w > tc.n {
			t.Errorf("trunc(%q, %d) display width = %d, want <= %d", tc.s, tc.n, w, tc.n)
		}
	}
}

// pad must align a clamped cell (multi-byte ellipsis) with an unclamped one:
// both render at the same display width.
func TestPadAlignsClampedCells(t *testing.T) {
	clamped := trunc("a-much-longer-string", 10)
	plain := "short"
	if cw, pw := cellWidth(pad(clamped, 22)), cellWidth(pad(plain, 22)); cw != pw {
		t.Errorf("padded widths differ: clamped=%d plain=%d", cw, pw)
	}
	if got := pad("abc", 5); got != "abc  " {
		t.Errorf("pad(abc, 5) = %q", got)
	}
	if got := pad("abcdef", 5); got != "abcdef" {
		t.Errorf("pad must not clip: got %q", got)
	}
}

// Backoff already elapsed (or no backoff) must not print a negative/zero
// "retry in 0s" countdown.
func TestRenderStatusNoRetryWhenBackoffElapsed(t *testing.T) {
	var buf bytes.Buffer
	c := &ctl{out: &buf}
	c.renderStatus(&runnyv1.GetStatusResponse{
		DaemonStarted: timestamppb.New(time.Now()),
		Slots: []*runnyv1.SlotStatus{{
			Slot:           "mac-1",
			State:          runnyv1.SlotState_SLOT_STATE_BACKOFF,
			StateEntered:   timestamppb.New(time.Now().Add(-30 * time.Second)),
			BackoffSeconds: 5, // long since elapsed
		}},
	})
	if strings.Contains(buf.String(), "retry in") {
		t.Errorf("printed a retry countdown for elapsed backoff:\n%s", buf.String())
	}
}
