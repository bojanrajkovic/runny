package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

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
