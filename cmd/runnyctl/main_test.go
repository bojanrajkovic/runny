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

// The IMAGE cell's one rule: the @hex12 suffix appears iff the wire carried
// image_digest — iff resolution completed this cycle. In particular a
// configured digest pin must never be echoed into the cell (the tripwire
// cases): a pinned-config fleet crash-looping against a dead registry must
// not render the same cell as a healthy one.
func TestImageCell(t *testing.T) {
	const digest = "sha256:ab12cd34ef567890ab12cd34ef567890ab12cd34ef567890ab12cd34ef567890"
	cases := []struct {
		name        string
		ref, digest string
		want        string
	}{
		{"tag ref, resolved", "ghcr.io/cirruslabs/macos-tahoe-xcode:26.3", digest, "macos-tahoe-xcode:26.3@ab12cd34ef56"},
		{"tag ref, unresolved", "ghcr.io/cirruslabs/macos-tahoe-xcode:26.3", "", "macos-tahoe-xcode:26.3"},
		{"pinned ref, unresolved — the echoed-pin tripwire", "ghcr.io/cirruslabs/macos-tahoe-xcode@" + digest, "", "macos-tahoe-xcode"},
		{"pinned ref, resolved — no doubled @", "ghcr.io/cirruslabs/macos-tahoe-xcode@" + digest, digest, "macos-tahoe-xcode@ab12cd34ef56"},
		{"ref with no slash", "image:1", digest, "image:1@ab12cd34ef56"},
		{"digest without sha256: prefix", "img:1", "ab12cd34ef567890", "img:1@ab12cd34ef56"},
		{"digest shorter than 12 hex", "img:1", "abcd", "img:1@abcd"},
		{"ref base longer than the clamp keeps the whole digest", "registry.example/very-long-image-name-overflowing-the-clamp:tag", digest, "very-long-image-name-ove…@ab12cd34ef56"},
		{"empty everything", "", "", ""},
	}
	for _, tc := range cases {
		if got := imageCell(tc.ref, tc.digest); got != tc.want {
			t.Errorf("%s: imageCell(%q, %q) = %q, want %q", tc.name, tc.ref, tc.digest, got, tc.want)
		}
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

// The IMAGE column is always on: a LISTENING slot with a resolved digest
// shows the compact name:tag@hex12 cell; a BACKOFF slot (digest cleared —
// no guest exists) shows the bare ref with no @ suffix.
func TestRenderStatusImageColumn(t *testing.T) {
	var buf bytes.Buffer
	c := &ctl{out: &buf}
	c.renderStatus(&runnyv1.GetStatusResponse{
		Version:       "test",
		DaemonStarted: timestamppb.New(time.Now()),
		Slots: []*runnyv1.SlotStatus{
			{
				Slot:         "mac-1",
				State:        runnyv1.SlotState_SLOT_STATE_LISTENING,
				StateEntered: timestamppb.New(time.Now()),
				Image:        "ghcr.io/test/macos-tahoe-xcode:26.3",
				ImageDigest:  "sha256:ab12cd34ef567890ab12cd34ef567890",
			},
			{
				Slot:         "mac-2",
				State:        runnyv1.SlotState_SLOT_STATE_BACKOFF,
				StateEntered: timestamppb.New(time.Now()),
				Image:        "ghcr.io/test/macos-tahoe-xcode:26.3",
				// No digest: BACKOFF cleared it.
			},
		},
	})
	out := buf.String()
	lines := strings.Split(out, "\n")
	if !strings.Contains(lines[2], "IMAGE") {
		t.Errorf("header missing IMAGE column:\n%s", out)
	}
	var mac1, mac2 string
	for _, line := range lines {
		if strings.Contains(line, "mac-1") {
			mac1 = line
		}
		if strings.Contains(line, "mac-2") {
			mac2 = line
		}
	}
	if !strings.Contains(mac1, "macos-tahoe-xcode:26.3@ab12cd34ef56") {
		t.Errorf("LISTENING slot missing compact image cell: %q", mac1)
	}
	if !strings.Contains(mac2, "macos-tahoe-xcode:26.3") || strings.Contains(mac2, "@") {
		t.Errorf("BACKOFF slot must show the bare ref with no @ suffix: %q", mac2)
	}
}

// A row whose IMAGE cell needs the clamp must keep its JOB content at the
// same display offset as unclamped rows — the alignment regression the
// rune-aware trunc + display-width pad exist to prevent.
func TestRenderStatusClampedImageAlignment(t *testing.T) {
	const digest = "sha256:ab12cd34ef567890ab12cd34ef567890"
	var buf bytes.Buffer
	c := &ctl{out: &buf}
	now := timestamppb.New(time.Now())
	c.renderStatus(&runnyv1.GetStatusResponse{
		DaemonStarted: now,
		Slots: []*runnyv1.SlotStatus{
			{
				Slot: "mac-1", State: runnyv1.SlotState_SLOT_STATE_JOB, StateEntered: now,
				Image: "ghcr.io/test/short:1", ImageDigest: digest,
				Job: &runnyv1.JobInfo{Name: "job-a", Started: now},
			},
			{
				Slot: "mac-2", State: runnyv1.SlotState_SLOT_STATE_JOB, StateEntered: now,
				Image: "ghcr.io/test/very-long-image-name-overflowing-the-clamp:26.3", ImageDigest: digest,
				Job: &runnyv1.JobInfo{Name: "job-b", Started: now},
			},
		},
	})
	// Display offset = rune offset: the table is ASCII except the one-column
	// ellipsis rune inside clamped cells.
	runeOffset := func(line, sub string) int {
		i := strings.Index(line, sub)
		if i < 0 {
			return -1
		}
		return utf8.RuneCountInString(line[:i])
	}
	offA, offB := -1, -1
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "job-a") {
			offA = runeOffset(line, "job-a")
		}
		if strings.Contains(line, "job-b") {
			offB = runeOffset(line, "job-b")
		}
	}
	if offA < 0 || offB < 0 {
		t.Fatalf("job cells not rendered:\n%s", buf.String())
	}
	if offA != offB {
		t.Errorf("JOB display offsets diverge: unclamped row %d, clamped row %d\n%s", offA, offB, buf.String())
	}
}

// renderCycle shows the configured ref (intent) beside the digest (truth);
// a record written by an older daemon (no ref) keeps the digest-only form.
func TestRenderCycleShowsImageRef(t *testing.T) {
	now := timestamppb.New(time.Now())
	var buf bytes.Buffer
	c := &ctl{out: &buf}
	c.renderCycle(&runnyv1.CycleRecord{
		CycleId: "abcd1234", Slot: "mac-1", Result: "success",
		Image: "ghcr.io/test/image:1", ImageDigest: "sha256:fake",
		Started: now, Finished: now,
	})
	if !strings.Contains(buf.String(), "image ghcr.io/test/image:1 @ sha256:fake") {
		t.Errorf("ref+digest form missing:\n%s", buf.String())
	}

	buf.Reset()
	c.renderCycle(&runnyv1.CycleRecord{
		CycleId: "abcd1234", Slot: "mac-1", Result: "success",
		ImageDigest: "sha256:fake",
		Started:     now, Finished: now,
	})
	if !strings.Contains(buf.String(), "image sha256:fake") || strings.Contains(buf.String(), " @ ") {
		t.Errorf("old-daemon record must render digest-only:\n%s", buf.String())
	}

	buf.Reset()
	c.renderCycle(&runnyv1.CycleRecord{
		CycleId: "abcd1234", Slot: "mac-1", Result: "failure",
		Image:   "ghcr.io/test/image:1",
		Started: now, Finished: now,
	})
	if !strings.Contains(buf.String(), "image ghcr.io/test/image:1 |") || strings.Contains(buf.String(), "@") {
		t.Errorf("failed-before-resolve record must render ref-only, no dangling '@':\n%s", buf.String())
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
