package socket

import (
	"testing"
	"time"

	"github.com/bojanrajkovic/runny/internal/logring"
	"github.com/bojanrajkovic/runny/internal/statemachine"
	runnyv1 "github.com/bojanrajkovic/runny/proto/runny/v1"
)

// allStates mirrors the FSM's state inventory. If a state is added there
// without a proto mapping, statusToProto silently degrades it to
// SLOT_STATE_UNSPECIFIED on the wire — this test makes that loud.
var allStates = []statemachine.State{
	statemachine.StateBackoff,
	statemachine.StateEnsureImage,
	statemachine.StateClone,
	statemachine.StateBoot,
	statemachine.StateAwaitIP,
	statemachine.StateAwaitSSH,
	statemachine.StateMintJIT,
	statemachine.StateProvision,
	statemachine.StateListening,
	statemachine.StateJob,
	statemachine.StateTeardown,
}

func TestStateToProtoIsExhaustive(t *testing.T) {
	if len(stateToProto) != len(allStates) {
		t.Errorf("stateToProto has %d entries, FSM has %d states", len(stateToProto), len(allStates))
	}
	for _, st := range allStates {
		if pb, ok := stateToProto[st]; !ok || pb == runnyv1.SlotState_SLOT_STATE_UNSPECIFIED {
			t.Errorf("state %s has no proto mapping (would render as UNSPECIFIED on the wire)", st)
		}
	}
}

func TestStatusToProtoCarriesWedgedAndDetail(t *testing.T) {
	st := statemachine.Status{
		Slot:    "mac-1",
		State:   statemachine.StateTeardown,
		Detail:  "guest survived force-stop",
		Wedged:  true,
		CycleID: "abcd1234",
	}
	pb := statusToProto(st)
	if !pb.GetWedged() || pb.GetDetail() != st.Detail || pb.GetSlot() != "mac-1" {
		t.Errorf("statusToProto dropped fields: %+v", pb)
	}
}

func TestToLogLine(t *testing.T) {
	e := logring.Entry{
		Time:    time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC),
		Level:   "INFO",
		Message: "state",
		Attrs:   map[string]string{"slot": "mac-1"},
	}
	l := toLogLine(e)
	if l.GetMessage() != "state" || l.GetLevel() != "INFO" || l.GetAttrs()["slot"] != "mac-1" {
		t.Errorf("toLogLine dropped fields: %+v", l)
	}
	if !l.GetTime().AsTime().Equal(e.Time) {
		t.Errorf("time mangled: %v", l.GetTime().AsTime())
	}
}
