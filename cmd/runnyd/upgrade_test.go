package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// observe logs the upgrade hint only on the TRANSITION into "behind" for a given
// target core — not every tick — and re-arms when the daemon is no longer behind,
// so a later upgrade logs again. This pins the dedup/transition logic without a
// real binary.
func TestUpgradeNoticeObserve(t *testing.T) {
	var buf bytes.Buffer
	u := &upgradeNotice{
		log:     slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		running: "0.6.0",
	}
	step := func(target string, want int) {
		t.Helper()
		u.observe(target)
		if got := strings.Count(buf.String(), "a newer runnyd is available"); got != want {
			t.Fatalf("after observe(%q): %d notices, want %d", target, got, want)
		}
	}
	step("0.6.0", 0) // equal: not behind
	step("0.7.0", 1) // behind: log
	step("0.7.0", 1) // same target: dedup, no re-log
	step("", 1)      // unknown target: stay quiet, keep state
	step("0.8.0", 2) // a newer binary landed: log again
	step("0.6.0", 2) // on-disk binary went back to equal: not behind, re-arm
	step("0.8.0", 3) // behind again after re-arm: log
}

// An unstamped dev build (no version core) must never wear a false "you're
// behind", mirroring the CLI skew check's quiet-on-dev branch.
func TestUpgradeNoticeQuietWhenUnstamped(t *testing.T) {
	var buf bytes.Buffer
	u := &upgradeNotice{log: slog.New(slog.NewTextHandler(&buf, nil)), running: ""}
	u.observe("9.9.9")
	if buf.Len() != 0 {
		t.Errorf("unstamped build logged %q, want silence", buf.String())
	}
}
