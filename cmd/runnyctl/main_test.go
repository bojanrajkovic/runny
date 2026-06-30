package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
		// A clamped output occupies exactly n columns (the property the
		// padding depends on); an unclamped input passes through untouched.
		w := utf8.RuneCountInString(got)
		if utf8.RuneCountInString(tc.s) > tc.n && w != tc.n {
			t.Errorf("trunc(%q, %d) clamped width = %d, want exactly %d", tc.s, tc.n, w, tc.n)
		}
		if w > tc.n {
			t.Errorf("trunc(%q, %d) display width = %d, want <= %d", tc.s, tc.n, w, tc.n)
		}
	}
}

// cellWidth and trunc must measure terminal DISPLAY columns, not rune count:
// the JOB/NOTE columns carry GitHub-controlled job display names, which can hold
// double-width CJK runes (issue #51) — a job named in the workflow's `name:`
// field reaches runny verbatim via the runner's "Running job:" line. A rune-count
// budget under-counts a wide cell and shifts every column to its right.
func TestCellWidthCountsDisplayColumns(t *testing.T) {
	cases := []struct {
		s    string
		want int
	}{
		{"build", 5},
		{"ビル", 4},        // two wide (CJK) runes = 4 columns
		{"ビルドとテスト", 14},  // 7 wide runes
		{"测试-linux", 10}, // 2 wide + 6 ascii
		{"deploy", 6},
		{"café", 4},     // precomposed é is one column, not ambiguous-wide
		{"❤️", 2},       // emoji-presentation (VS16): two columns, not one
		{"⚠️ tests", 8}, // VS16 emoji (2) + " tests" (6)
	}
	for _, tc := range cases {
		if got := cellWidth(tc.s); got != tc.want {
			t.Errorf("cellWidth(%q) = %d, want %d", tc.s, got, tc.want)
		}
	}
}

// trunc must clamp by display width: the result occupies at most n columns (the
// property pad depends on), even when the input mixes wide and narrow runes.
func TestTruncClampsByDisplayWidth(t *testing.T) {
	cases := []struct {
		s string
		n int
	}{
		{"ビルドとテスト macos", 10},
		{"测试测试测试测试", 7},
		{"plain-ascii-string", 8},
		{"ビル", 4},     // exactly fits — no clamp
		{"ビルド", 4},    // 6 cols into 4 — clamps on a wide-rune boundary
		{"❤️❤️❤️", 4}, // VS16 emoji clusters must not be split mid-cluster
	}
	for _, tc := range cases {
		got := trunc(tc.s, tc.n)
		if w := cellWidth(got); w > tc.n {
			t.Errorf("trunc(%q, %d) = %q has display width %d > %d", tc.s, tc.n, got, w, tc.n)
		}
	}
}

// The real #51 scenario end to end: a slot running a CJK-named job must not
// shift the NOTE column relative to an ASCII-named one. The JOB cell is padded
// to a fixed DISPLAY width, so the wide runes can't push everything right.
func TestRenderStatusWideJobAligns(t *testing.T) {
	var buf bytes.Buffer
	c := &ctl{out: &buf}
	now := timestamppb.New(time.Now())
	c.renderStatus(&runnyv1.GetStatusResponse{
		DaemonStarted: now,
		Slots: []*runnyv1.SlotStatus{
			{
				Slot: "mac-1", State: runnyv1.SlotState_SLOT_STATE_JOB, StateEntered: now,
				Job: &runnyv1.JobInfo{Name: "build", Started: now}, LastFailure: "NOTEMARK",
			},
			{
				Slot: "mac-2", State: runnyv1.SlotState_SLOT_STATE_JOB, StateEntered: now,
				Job: &runnyv1.JobInfo{Name: "ビルドとテスト", Started: now}, LastFailure: "NOTEMARK",
			},
		},
	})
	// Offsets are DISPLAY columns (cellWidth of the prefix), not byte/rune index.
	displayOffset := func(line, sub string) int {
		i := strings.Index(line, sub)
		if i < 0 {
			return -1
		}
		return cellWidth(line[:i])
	}
	off1, off2 := -1, -1
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "mac-1") {
			off1 = displayOffset(line, "NOTEMARK")
		}
		if strings.Contains(line, "mac-2") {
			off2 = displayOffset(line, "NOTEMARK")
		}
	}
	if off1 < 0 || off2 < 0 {
		t.Fatalf("NOTE not rendered in both rows:\n%s", buf.String())
	}
	if off1 != off2 {
		t.Errorf("NOTE display offset differs: ascii-job row %d, wide-job row %d\n%s", off1, off2, buf.String())
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

	// A configured "@sha256:" pin must be stripped from the intent half — the
	// status cell strips it, and why must too, or a pinned fleet shows a
	// full pin digest for a cycle that never resolved (and the digest twice
	// when it did). Resolving a pin returns the pin.
	const pin = "sha256:ab12cd34ef567890ab12cd34ef567890ab12cd34ef567890ab12cd34ef567890"
	buf.Reset()
	c.renderCycle(&runnyv1.CycleRecord{
		CycleId: "abcd1234", Slot: "mac-1", Result: "failure",
		Image:   "ghcr.io/test/image@" + pin, // pinned config, registry never reached
		Started: now, Finished: now,
	})
	if got := buf.String(); !strings.Contains(got, "image ghcr.io/test/image |") || strings.Contains(got, "sha256:") {
		t.Errorf("pinned-but-unresolved cycle must strip the pin, leaving no sha256: on screen:\n%s", got)
	}

	buf.Reset()
	c.renderCycle(&runnyv1.CycleRecord{
		CycleId: "abcd1234", Slot: "mac-1", Result: "success",
		Image:       "ghcr.io/test/image@" + pin, // pinned config, resolved to the pin
		ImageDigest: pin,
		Started:     now, Finished: now,
	})
	if got := buf.String(); !strings.Contains(got, "image ghcr.io/test/image @ sha256:ab12cd34ef56") || strings.Count(got, "sha256:") != 1 {
		t.Errorf("resolved pinned cycle must show the ref once and the short digest once, not the digest twice:\n%s", got)
	}
}

// A "warn" state outcome (a teardown that destroyed the guest but left a
// swept-later orphan) must render visibly in `why` — never be dropped the way
// a bare "ok" drops its error text, which would hide the cleanup failure.
func TestRenderCycleShowsWarnOutcome(t *testing.T) {
	now := timestamppb.New(time.Now())
	var buf bytes.Buffer
	c := &ctl{out: &buf}
	c.renderCycle(&runnyv1.CycleRecord{
		CycleId: "abcd1234", Slot: "mac-1", Result: "failure",
		Started: now, Finished: now,
		States: []*runnyv1.StateRecord{{
			State: runnyv1.SlotState_SLOT_STATE_TEARDOWN, Entered: now, Left: now,
			Outcome: "warn", Error: "deregister runner 101: github 500",
		}},
	})
	if got := buf.String(); !strings.Contains(got, "warn: deregister runner 101: github 500") {
		t.Errorf("warn outcome must render its detail in why:\n%s", got)
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

func TestUsageMentionsReload(t *testing.T) {
	if !strings.Contains(usage, "reload") {
		t.Error("usage does not mention the reload command")
	}
}

// Issue #47: a global flag (-json) must be honored AFTER the subcommand, not
// only before it — `runnyctl status -json` must match `runnyctl -json status`.
func TestDispatchTrailingJSONMatchesLeading(t *testing.T) {
	resp := &runnyv1.GetStatusResponse{Version: "test", DaemonStarted: timestamppb.New(time.Now())}
	render := func(setup func(*ctl), args ...string) string {
		var buf bytes.Buffer
		c := &ctl{client: &fakeRecycleClient{status: resp}, out: &buf, err: &bytes.Buffer{}}
		if setup != nil {
			setup(c)
		}
		if err := c.dispatch(context.Background(), args); err != nil {
			t.Fatalf("dispatch %v: %v", args, err)
		}
		return buf.String()
	}
	trailing := render(nil, "status", "-json")                  // runnyctl status -json
	leading := render(func(c *ctl) { c.json = true }, "status") // runnyctl -json status
	if trailing != leading {
		t.Errorf("trailing vs leading -json differ:\n%q\nvs\n%q", trailing, leading)
	}
	if !strings.HasPrefix(strings.TrimSpace(trailing), "{") {
		t.Errorf("status -json did not emit JSON:\n%s", trailing)
	}
}

// An unknown trailing flag must error rather than be swallowed (no-flagset
// command) or kill the process via os.Exit (a command with its own flagset).
func TestDispatchRejectsUnknownTrailingFlag(t *testing.T) {
	mk := func() *ctl {
		return &ctl{
			client: &fakeRecycleClient{status: &runnyv1.GetStatusResponse{
				Slots: []*runnyv1.SlotStatus{{
					Slot: "mac-1", State: runnyv1.SlotState_SLOT_STATE_LISTENING,
					StateEntered: timestamppb.New(time.Now()),
				}},
			}},
			out: &bytes.Buffer{}, err: &bytes.Buffer{},
		}
	}
	// A no-flagset command (status) and a slot command (recycle, via slotArg)
	// must both surface the bad flag as a returnable error naming it.
	for _, args := range [][]string{{"status", "-bogus"}, {"recycle", "mac-1", "-bogus"}} {
		err := mk().dispatch(context.Background(), args)
		if err == nil {
			t.Errorf("%v: unknown trailing flag was swallowed; expected an error", args)
			continue
		}
		if !strings.Contains(err.Error(), "bogus") {
			t.Errorf("%v: error should name the offending flag: %v", args, err)
		}
	}
}

// A stray positional on a no-argument command is not silently ignored — the
// same anti-swallow principle reload already applies to its own positionals.
func TestDispatchRejectsStrayPositional(t *testing.T) {
	c := &ctl{client: &fakeRecycleClient{status: &runnyv1.GetStatusResponse{}}, out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	if err := c.dispatch(context.Background(), []string{"status", "mac-1"}); err == nil {
		t.Error("status with a stray positional was swallowed; expected an error")
	}
}

// fakeLogStream EOFs immediately, so c.logs returns nil after parsing — letting
// the dispatch-level argument tests assert on parse acceptance, not streaming.
type fakeLogStream struct {
	grpc.ServerStreamingClient[runnyv1.LogLine]
}

func (fakeLogStream) Recv() (*runnyv1.LogLine, error) { return nil, io.EOF }

type fakeLogsClient struct {
	runnyv1.RunnyServiceClient
}

func (fakeLogsClient) StreamLogs(_ context.Context, _ *runnyv1.StreamLogsRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[runnyv1.LogLine], error) {
	return fakeLogStream{}, nil
}

// Issue #47, flag-first ordering: a trailing -json after `-flag SLOT` must still
// be honored, not rejected as a second positional. Go's flag package stops at
// the first positional, so slotArg must parse flags and positionals interspersed
// (the case Codex flagged on PR #106).
func TestSlotArgHonorsTrailingFlagAfterSlot(t *testing.T) {
	c := &ctl{out: &bytes.Buffer{}}
	fs, j := subFlags("recycle")
	force := fs.Bool("force", false, "")
	slot, err := c.slotArg(fs, j, []string{"-force", "mac-1", "-json"})
	if err != nil {
		t.Fatalf("recycle -force mac-1 -json: %v", err)
	}
	if slot != "mac-1" {
		t.Errorf("slot = %q, want mac-1", slot)
	}
	if !*force {
		t.Error("-force before the slot was not parsed")
	}
	if !c.json {
		t.Error("trailing -json after a flag-first slot was not folded")
	}
}

// logs' optional SLOT does not route through slotArg, yet a flag written AFTER
// the slot — the documented `logs [SLOT] [-daemon] [-replay N] [-follow=false]`
// form — must parse, not be rejected as a second positional (issue #47).
func TestDispatchLogsAcceptsFlagAfterSlot(t *testing.T) {
	for _, args := range [][]string{
		{"logs", "mac-1", "-replay", "10"},          // slot then flag (the broken form)
		{"logs", "mac-1", "-json"},                  // slot then -json
		{"logs", "-replay", "10", "mac-1"},          // flag then slot
		{"logs", "-replay", "10", "mac-1", "-json"}, // flag, slot, trailing -json (Codex's case)
		{"logs", "mac-1"},                           // bare slot
		{"logs"},                                    // no slot
	} {
		c := &ctl{client: fakeLogsClient{}, out: &bytes.Buffer{}, err: &bytes.Buffer{}}
		if err := c.dispatch(context.Background(), args); err != nil {
			t.Errorf("logs %v: unexpected error: %v", args[1:], err)
		}
	}
	// Two SLOT positionals is still an error.
	c := &ctl{client: fakeLogsClient{}, out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	if err := c.dispatch(context.Background(), []string{"logs", "mac-1", "mac-2"}); err == nil {
		t.Error("logs with two SLOT positionals should error")
	}
	// -daemon alongside a slot is still mutually exclusive (a different error,
	// not the positional-count one).
	c = &ctl{client: fakeLogsClient{}, out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	err := c.dispatch(context.Background(), []string{"logs", "mac-1", "-daemon"})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("logs mac-1 -daemon should fail as mutually exclusive; got %v", err)
	}
}

// A draining daemon shows a banner naming the cause and the slots the drain
// is still waiting on (anything not wedged and not paused-in-BACKOFF).
func TestRenderStatusShowsDrainingBanner(t *testing.T) {
	var buf bytes.Buffer
	c := &ctl{out: &buf}
	c.renderStatus(&runnyv1.GetStatusResponse{
		Version:       "test",
		DaemonStarted: timestamppb.New(time.Now()),
		Draining:      "config reload (rpc): new image",
		Slots: []*runnyv1.SlotStatus{
			{ // still draining: a job is running
				Slot:         "mac-1",
				State:        runnyv1.SlotState_SLOT_STATE_JOB,
				StateEntered: timestamppb.New(time.Now()),
			},
			{ // converged: paused in BACKOFF
				Slot:         "mac-2",
				State:        runnyv1.SlotState_SLOT_STATE_BACKOFF,
				StateEntered: timestamppb.New(time.Now()),
				Paused:       true,
			},
			{ // converged: wedged
				Slot:         "lin-1",
				State:        runnyv1.SlotState_SLOT_STATE_TEARDOWN,
				StateEntered: timestamppb.New(time.Now()),
				Wedged:       true,
			},
		},
	})
	out := buf.String()
	if !strings.Contains(out, "DRAINING: config reload (rpc): new image") {
		t.Errorf("no draining banner:\n%s", out)
	}
	if !strings.Contains(out, "waiting on: mac-1 (JOB)") {
		t.Errorf("banner does not name the holdout:\n%s", out)
	}
	for _, converged := range []string{"waiting on: mac-2", "mac-2 (", "lin-1 ("} {
		if strings.Contains(out, converged) {
			t.Errorf("banner lists a converged slot (%q):\n%s", converged, out)
		}
	}
}

func TestRenderStatusNoBannerWhenNotDraining(t *testing.T) {
	var buf bytes.Buffer
	c := &ctl{out: &buf}
	c.renderStatus(&runnyv1.GetStatusResponse{
		DaemonStarted: timestamppb.New(time.Now()),
		Slots: []*runnyv1.SlotStatus{{
			Slot:         "mac-1",
			State:        runnyv1.SlotState_SLOT_STATE_LISTENING,
			StateEntered: timestamppb.New(time.Now()),
		}},
	})
	if strings.Contains(buf.String(), "DRAINING") {
		t.Errorf("banner shown with no drain active:\n%s", buf.String())
	}
}

const testSHA = "4a5b6c7d8e9f00112233445566778899aabbccddeeff00112233445566778899"

// Acceptance renders everything from the response — slot count, truncated
// sha, the operator-paused-slots note, warnings — with no second RPC.
func TestRenderReloadAccepted(t *testing.T) {
	var buf bytes.Buffer
	c := &ctl{out: &buf}
	err := c.renderReload(&runnyv1.ReloadResponse{
		Accepted:            true,
		StartedDrain:        true,
		Draining:            "config reload (rpc): new image",
		SlotCount:           3,
		OperatorPausedSlots: []string{"mac-2", "lin-1"},
		ConfigSha256:        testSHA,
		Warnings: []*runnyv1.DoctorCheck{
			{Name: "local-network", Ok: false, Detail: "cannot reach the guest subnet"},
		},
	})
	if err != nil {
		t.Fatalf("accepted reload returned error: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"reload accepted: config validated (sha256 4a5b6c7d8e9f); draining 3 slot(s)",
		"running jobs finish first",
		"note: operator-paused slots resume after the respawn: mac-2, lin-1",
		"warning: local-network — cannot reach the guest subnet",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("accepted output missing %q:\n%s", want, out)
		}
	}
}

func TestRenderReloadAcceptedNoPausedNote(t *testing.T) {
	var buf bytes.Buffer
	c := &ctl{out: &buf}
	if err := c.renderReload(&runnyv1.ReloadResponse{
		Accepted:     true,
		StartedDrain: true,
		Draining:     "config reload (rpc)",
		SlotCount:    1,
		ConfigSha256: testSHA,
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "operator-paused") {
		t.Errorf("paused-slots note printed with an empty list:\n%s", buf.String())
	}
}

// Accepted while another drain was already running: the config is validated
// but THIS call did not start the drain — say which drain will apply it.
func TestRenderReloadAcceptedAlreadyDraining(t *testing.T) {
	var buf bytes.Buffer
	c := &ctl{out: &buf}
	err := c.renderReload(&runnyv1.ReloadResponse{
		Accepted:     true,
		StartedDrain: false, // a wedge drain was already running
		Draining:     "wedged guest: a VM survived force-stop (see the slot's cycle record)",
		ConfigSha256: testSHA,
	})
	if err != nil {
		t.Fatalf("accepted reload returned error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "daemon already draining (wedged guest:") ||
		!strings.Contains(out, "the respawn will apply this config") {
		t.Errorf("already-draining acceptance not rendered:\n%s", out)
	}
	if strings.Contains(out, "reload accepted: config validated") {
		t.Errorf("already-draining acceptance claimed to have started the drain:\n%s", out)
	}
}

// Refusal renders the failed checks with the doctor table and exits
// non-zero with the daemon-unchanged summary.
func TestRenderReloadRefused(t *testing.T) {
	var buf bytes.Buffer
	c := &ctl{out: &buf}
	err := c.renderReload(&runnyv1.ReloadResponse{
		Accepted: false,
		FailedChecks: []*runnyv1.DoctorCheck{
			{Name: "config-parse", Ok: false, Detail: "yaml: line 3: did not find expected node content"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "the running daemon is unchanged") {
		t.Errorf("refusal error = %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "config-parse") || !strings.Contains(out, "FAIL") {
		t.Errorf("refusal did not render the check table:\n%s", out)
	}
	if strings.Contains(out, "WARNING: the daemon is already draining") {
		t.Errorf("no-drain refusal printed the mid-drain warning:\n%s", out)
	}
}

// Refusal while a drain is active must scream: the respawn WILL load the
// invalid file.
func TestRenderReloadRefusedWhileDraining(t *testing.T) {
	var buf bytes.Buffer
	c := &ctl{out: &buf}
	err := c.renderReload(&runnyv1.ReloadResponse{
		Accepted: false,
		Draining: "config reload (SIGHUP)",
		FailedChecks: []*runnyv1.DoctorCheck{
			{Name: "config-parse", Ok: false, Detail: "bad yaml"},
		},
	})
	if err == nil {
		t.Error("refused reload returned nil error")
	}
	out := buf.String()
	if !strings.Contains(out, "WARNING: the daemon is already draining (config reload (SIGHUP)) and the respawn WILL load this invalid config") {
		t.Errorf("mid-drain refusal warning missing:\n%s", out)
	}
}

// fakeClient stubs just the RPCs a test drives; everything else panics via
// the embedded nil interface.
type fakeClient struct {
	runnyv1.RunnyServiceClient
	pauseResp *runnyv1.PauseResponse
	pruneResp *runnyv1.PruneResponse
	pruneErr  error
}

func (f *fakeClient) Pause(ctx context.Context, in *runnyv1.PauseRequest, opts ...grpc.CallOption) (*runnyv1.PauseResponse, error) {
	return f.pauseResp, nil
}

func (f *fakeClient) Prune(ctx context.Context, in *runnyv1.PruneRequest, opts ...grpc.CallOption) (*runnyv1.PruneResponse, error) {
	return f.pruneResp, f.pruneErr
}

func TestPausePrintsNote(t *testing.T) {
	var buf bytes.Buffer
	c := &ctl{out: &buf, client: &fakeClient{pauseResp: &runnyv1.PauseResponse{
		Note: "daemon is draining for restart (config reload (rpc)); pause is in-memory and will not survive the respawn",
	}}}
	if err := c.pause(context.Background(), "mac-1"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "note: daemon is draining for restart") {
		t.Errorf("pause note not printed:\n%s", buf.String())
	}

	buf.Reset()
	c.client = &fakeClient{pauseResp: &runnyv1.PauseResponse{}}
	if err := c.pause(context.Background(), "mac-1"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "note:") {
		t.Errorf("empty note printed a note line:\n%s", buf.String())
	}
}

// A DEBUG slot shows its auto-release countdown; an armed JOB slot shows the
// hold is armed (issue #39).
func TestRenderStatusDebugAndArmed(t *testing.T) {
	var buf bytes.Buffer
	c := &ctl{out: &buf}
	c.renderStatus(&runnyv1.GetStatusResponse{
		Version:       "test",
		DaemonStarted: timestamppb.New(time.Now().Add(-time.Hour)),
		Slots: []*runnyv1.SlotStatus{
			{
				Slot:             "mac-1",
				State:            runnyv1.SlotState_SLOT_STATE_DEBUG,
				StateEntered:     timestamppb.New(time.Now()),
				DebugHoldExpires: timestamppb.New(time.Now().Add(90 * time.Minute)),
			},
			{
				Slot:           "mac-2",
				State:          runnyv1.SlotState_SLOT_STATE_JOB,
				StateEntered:   timestamppb.New(time.Now()),
				DebugHoldArmed: true,
				Job:            &runnyv1.JobInfo{Name: "build"},
			},
		},
	})
	out := buf.String()
	if !strings.Contains(out, "DEBUG") || !strings.Contains(out, "auto-releases in") {
		t.Errorf("DEBUG slot did not render countdown:\n%s", out)
	}
	if !strings.Contains(out, "debug hold armed") {
		t.Errorf("armed JOB slot did not render the armed note:\n%s", out)
	}
}

// why's contamination line: the job ran with operator keys, and each attempt's
// state + outcome is rendered.
func TestRenderCycleContamination(t *testing.T) {
	var buf bytes.Buffer
	c := &ctl{out: &buf}
	c.renderCycle(&runnyv1.CycleRecord{
		CycleId: "abcd1234", Slot: "mac-1", Result: "failure",
		FailureState: "JOB", FailureError: "exceeded budget",
		Started:  timestamppb.New(time.Now()),
		Finished: timestamppb.New(time.Now()),
		Job:      &runnyv1.JobInfo{Name: "build", OperatorKeys: []string{"SHA256:abc"}},
		InjectedKeys: []*runnyv1.InjectedKey{
			{Fingerprint: "SHA256:abc", Outcome: "armed", State: "JOB", Reason: "wedged"},
		},
	})
	out := buf.String()
	if !strings.Contains(out, "ran with operator key(s) SHA256:abc") {
		t.Errorf("job line missing operator-key contamination:\n%s", out)
	}
	if !strings.Contains(out, "debug key") || !strings.Contains(out, "[JOB]") || !strings.Contains(out, "armed") {
		t.Errorf("injected-key line missing state/outcome:\n%s", out)
	}
}

// fakeRecycleClient embeds the generated client (so it satisfies the full
// interface) and overrides only GetStatus and Recycle to exercise the recycle
// guard (decision 14/15).
type fakeRecycleClient struct {
	runnyv1.RunnyServiceClient
	status    *runnyv1.GetStatusResponse
	statusErr error
	recycled  *runnyv1.RecycleRequest
}

func (f *fakeRecycleClient) GetStatus(_ context.Context, _ *runnyv1.GetStatusRequest, _ ...grpc.CallOption) (*runnyv1.GetStatusResponse, error) {
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	return f.status, nil
}

func (f *fakeRecycleClient) Recycle(_ context.Context, req *runnyv1.RecycleRequest, _ ...grpc.CallOption) (*runnyv1.RecycleResponse, error) {
	f.recycled = req
	return &runnyv1.RecycleResponse{}, nil
}

func TestRecycleGuardsDebugAndJob(t *testing.T) {
	statusWith := func(state runnyv1.SlotState) *runnyv1.GetStatusResponse {
		return &runnyv1.GetStatusResponse{Slots: []*runnyv1.SlotStatus{{
			Slot: "mac-1", State: state, StateEntered: timestamppb.New(time.Now()),
			Job: &runnyv1.JobInfo{Name: "build"},
		}}}
	}

	// DEBUG without -force is refused, with no Recycle sent.
	fc := &fakeRecycleClient{status: statusWith(runnyv1.SlotState_SLOT_STATE_DEBUG)}
	c := &ctl{client: fc, out: &bytes.Buffer{}}
	if err := c.recycle(context.Background(), "mac-1", "x", false); err == nil {
		t.Error("DEBUG recycle without -force should be refused")
	}
	if fc.recycled != nil {
		t.Error("a refused DEBUG recycle must not send Recycle")
	}

	// JOB without -force is refused.
	fc = &fakeRecycleClient{status: statusWith(runnyv1.SlotState_SLOT_STATE_JOB)}
	c = &ctl{client: fc, out: &bytes.Buffer{}}
	if err := c.recycle(context.Background(), "mac-1", "x", false); err == nil {
		t.Error("JOB recycle without -force should be refused")
	}
	if fc.recycled != nil {
		t.Error("a refused JOB recycle must not send Recycle")
	}

	// JOB with -force sets cancel_running_job (the operator OBSERVED JOB).
	fc = &fakeRecycleClient{status: statusWith(runnyv1.SlotState_SLOT_STATE_JOB)}
	c = &ctl{client: fc, out: &bytes.Buffer{}}
	if err := c.recycle(context.Background(), "mac-1", "x", true); err != nil {
		t.Fatalf("forced JOB recycle: %v", err)
	}
	if fc.recycled == nil || !fc.recycled.GetCancelRunningJob() {
		t.Errorf("forced JOB recycle did not set cancel_running_job: %+v", fc.recycled)
	}

	// LISTENING needs no force and never sets cancel_running_job.
	fc = &fakeRecycleClient{status: statusWith(runnyv1.SlotState_SLOT_STATE_LISTENING)}
	c = &ctl{client: fc, out: &bytes.Buffer{}}
	if err := c.recycle(context.Background(), "mac-1", "x", false); err != nil {
		t.Fatalf("LISTENING recycle: %v", err)
	}
	if fc.recycled == nil || fc.recycled.GetCancelRunningJob() {
		t.Errorf("LISTENING recycle must not cancel a job: %+v", fc.recycled)
	}

	// GetStatus failure without -force REFUSES rather than silently degrading:
	// the guard cannot tell whether the recycle would destroy a debug hold, and
	// a plain recycle releases a hold daemon-side regardless.
	fc = &fakeRecycleClient{statusErr: errors.New("status rpc blip")}
	c = &ctl{client: fc, out: &bytes.Buffer{}}
	if err := c.recycle(context.Background(), "mac-1", "x", false); err == nil {
		t.Error("recycle should refuse when status is unreadable and -force is absent")
	}
	if fc.recycled != nil {
		t.Error("a status-blip recycle without -force must not send Recycle")
	}

	// GetStatus failure WITH -force proceeds: the operator consented to whatever
	// shape the slot is in.
	fc = &fakeRecycleClient{statusErr: errors.New("status rpc blip")}
	c = &ctl{client: fc, out: &bytes.Buffer{}}
	if err := c.recycle(context.Background(), "mac-1", "x", true); err != nil {
		t.Fatalf("forced recycle through a status blip: %v", err)
	}
	if fc.recycled == nil {
		t.Error("a forced recycle through a status blip must still send Recycle")
	}
	if fc.recycled.GetCancelRunningJob() {
		t.Error("a forced recycle with unreadable status must not blindly cancel a job")
	}
}

// shouldHint is the decision: only a transport/dial failure (the daemon never
// answered this invocation) earns the hint. codes.Unavailable is overloaded — a
// LIVE daemon also returns it for application conditions ("slot is not accepting
// commands"), and a stream that broke after connecting surfaces it too — so an
// Unavailable over a daemon that answered (a Ready conn, OR a stream that
// received a record; both fold into daemonAnswered) must NOT be hinted. A
// non-Unavailable error never is.
func TestShouldHint(t *testing.T) {
	unavail := status.Error(codes.Unavailable, "x")
	if !shouldHint(unavail, false) {
		t.Error("a transport-time Unavailable (daemon never answered) must be hinted")
	}
	if shouldHint(unavail, true) {
		t.Error("an Unavailable after the daemon answered (app-level, or mid-stream) must NOT be hinted")
	}
	if shouldHint(status.Error(codes.FailedPrecondition, "x"), false) {
		t.Error("a non-Unavailable error must never be hinted")
	}
	if shouldHint(nil, false) {
		t.Error("nil must never be hinted")
	}
}

// connHint is the wording layer (shouldHint having decided to hint): a missing
// socket raises the different-home possibility; a present-but-silent one says
// the daemon isn't answering. It stays total — a non-Unavailable or nil error
// passes through unchanged.
func TestConnHintNamesSocketAndHomeWhenSocketAbsent(t *testing.T) {
	base := status.Error(codes.Unavailable, `connection error: dial unix /home/x/.runny/runnyd.sock: connect: no such file or directory`)
	got := connHint(base, "/home/x/.runny/runnyd.sock", false)
	if got == nil {
		t.Fatal("expected a wrapped error, got nil")
	}
	msg := got.Error()
	if !strings.Contains(msg, "/home/x/.runny/runnyd.sock") {
		t.Errorf("hint must name the resolved socket path; got: %s", msg)
	}
	if !strings.Contains(msg, "different home") {
		t.Errorf("missing-socket hint must raise the different-home possibility; got: %s", msg)
	}
	if !errors.Is(got, base) {
		t.Error("connHint must wrap the original error, not replace it")
	}
}

func TestConnHintSaysNotAnsweringWhenSocketPresent(t *testing.T) {
	base := status.Error(codes.Unavailable, "connection refused")
	msg := connHint(base, "/home/x/.runny/runnyd.sock", true).Error()
	if !strings.Contains(msg, "isn't answering") {
		t.Errorf("present-socket hint must say the daemon isn't answering; got: %s", msg)
	}
	if strings.Contains(msg, "different home") {
		t.Errorf("a present socket rules out a home mismatch; got: %s", msg)
	}
}

func TestConnHintPassesThroughNonUnavailable(t *testing.T) {
	refusal := status.Error(codes.FailedPrecondition, "2 check(s) failed")
	if got := connHint(refusal, "/x", false); got.Error() != refusal.Error() {
		t.Errorf("a non-Unavailable error must pass through unchanged; got: %s", got.Error())
	}
	if got := connHint(nil, "/x", false); got != nil {
		t.Errorf("nil must pass through as nil; got: %v", got)
	}
}

// status mirrors the app's grant card for the headless operator: a DENIED grant
// (runnyd can't reach the guest subnet) is loud, UNKNOWN is a proactive note,
// and the healthy/absent states stay quiet so routine status output isn't
// cluttered.
func TestLocalNetworkNote(t *testing.T) {
	if s := localNetworkNote(runnyv1.LocalNetworkGrant_LOCAL_NETWORK_GRANT_DENIED); !strings.Contains(s, "DENIED") {
		t.Errorf("DENIED note = %q, want it to flag the denial", s)
	}
	if s := localNetworkNote(runnyv1.LocalNetworkGrant_LOCAL_NETWORK_GRANT_UNKNOWN); s == "" {
		t.Error("UNKNOWN should surface a proactive note")
	} else if strings.Contains(s, "no guest has booted") {
		// UNKNOWN also fires when a vmnet interface IS up but the gateway probe
		// times out — asserting "no guest has booted" would misdirect an operator
		// chasing a live network problem. The note must not claim a specific cause.
		t.Errorf("UNKNOWN note must not assert a specific cause; got %q", s)
	}
	// SELF_DAEMONIZED must be loud AND carry the launch-context remediation, NOT
	// the TCC-grant one (which is a dead end for a mislaunched daemon).
	if s := localNetworkNote(runnyv1.LocalNetworkGrant_LOCAL_NETWORK_GRANT_SELF_DAEMONIZED); s == "" {
		t.Error("SELF_DAEMONIZED should surface a note")
	} else if !strings.Contains(s, "launchd") || !strings.Contains(s, "foreground") {
		t.Errorf("SELF_DAEMONIZED note must give the launch-context fix; got %q", s)
	} else if strings.Contains(s, "grant it Local Network access") {
		t.Errorf("SELF_DAEMONIZED note must NOT render the dead-end TCC-grant fix; got %q", s)
	}
	if s := localNetworkNote(runnyv1.LocalNetworkGrant_LOCAL_NETWORK_GRANT_REACHABLE); s != "" {
		t.Errorf("REACHABLE should be quiet; got %q", s)
	}
	if s := localNetworkNote(runnyv1.LocalNetworkGrant_LOCAL_NETWORK_GRANT_UNSPECIFIED); s != "" {
		t.Errorf("UNSPECIFIED (old/non-darwin daemon) should be quiet; got %q", s)
	}
}

func TestRenderStatusShowsDeniedLocalNetwork(t *testing.T) {
	var buf bytes.Buffer
	c := &ctl{out: &buf}
	c.renderStatus(&runnyv1.GetStatusResponse{
		Version:           "test",
		DaemonStarted:     timestamppb.New(time.Now()),
		LocalNetworkGrant: runnyv1.LocalNetworkGrant_LOCAL_NETWORK_GRANT_DENIED,
	})
	if !strings.Contains(buf.String(), "DENIED") {
		t.Errorf("status did not surface a denied Local Network grant:\n%s", buf.String())
	}
}

func TestRenderStatusQuietWhenLocalNetworkReachable(t *testing.T) {
	var buf bytes.Buffer
	c := &ctl{out: &buf}
	c.renderStatus(&runnyv1.GetStatusResponse{
		Version:           "test",
		DaemonStarted:     timestamppb.New(time.Now()),
		LocalNetworkGrant: runnyv1.LocalNetworkGrant_LOCAL_NETWORK_GRANT_REACHABLE,
	})
	if strings.Contains(strings.ToLower(buf.String()), "local network") {
		t.Errorf("a reachable grant should add no line; got:\n%s", buf.String())
	}
}

// execSSH with noExec=true must return nil immediately — the operator opted out
// of the auto-exec and wants print-only behavior.
func TestExecSSHNoExecReturnsNil(t *testing.T) {
	c := &ctl{out: &bytes.Buffer{}}
	resp := &runnyv1.InjectDebugKeyResponse{
		User: "admin", Ip: "192.168.64.2",
		HostKeys: []string{"192.168.64.2 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAItest"},
	}
	if err := c.execSSH(resp, true); err != nil {
		t.Errorf("execSSH with noExec=true should return nil, got: %v", err)
	}
}

// prune must return a non-nil error (exit 1) when the daemon reports scan or
// apply errors so that scripts and CI pipelines can detect an incomplete prune.
func TestPruneExitsNonZeroOnErrors(t *testing.T) {
	var buf bytes.Buffer
	c := &ctl{out: &buf, client: &fakeClient{pruneResp: &runnyv1.PruneResponse{
		Errors: []string{"image-bundle scan: permission denied"},
	}}}
	if err := c.prune(context.Background(), false); err == nil {
		t.Fatal("expected error when daemon reports errors, got nil")
	}
}

// execSSH on a non-TTY stdin (always the case in test context) must return nil
// without attempting to exec — piped/scripted callers keep the print-only path.
func TestExecSSHNonTTYSkips(t *testing.T) {
	c := &ctl{out: &bytes.Buffer{}}
	resp := &runnyv1.InjectDebugKeyResponse{User: "admin", Ip: "192.168.64.2"}
	if err := c.execSSH(resp, false); err != nil {
		t.Errorf("execSSH on non-TTY stdin should return nil, got: %v", err)
	}
}
