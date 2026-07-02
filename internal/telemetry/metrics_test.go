package telemetry

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/bojanrajkovic/runny/internal/obs"
)

func newTestMetrics(t *testing.T) (obs.Emitter, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	emit, err := NewMetricsConsumer(mp.Meter("test"))
	if err != nil {
		t.Fatalf("NewMetricsConsumer: %v", err)
	}
	return emit, reader
}

func collect(t *testing.T, reader *sdkmetric.ManualReader) map[string]metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	out := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = m
		}
	}
	return out
}

// histPoint finds the single histogram datapoint whose attribute set matches
// want exactly, failing the test when it's absent or duplicated.
func histPoint(t *testing.T, m metricdata.Metrics, want attribute.Set) metricdata.HistogramDataPoint[float64] {
	t.Helper()
	h, ok := m.Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("%s: data is %T, want Histogram[float64]", m.Name, m.Data)
	}
	var found []metricdata.HistogramDataPoint[float64]
	for _, dp := range h.DataPoints {
		if dp.Attributes.Equals(&want) {
			found = append(found, dp)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s: %d datapoints match %v, want 1 (have %d total)", m.Name, len(found), want, len(h.DataPoints))
	}
	return found[0]
}

// sumPoint finds the single counter datapoint matching want exactly.
func sumPoint(t *testing.T, m metricdata.Metrics, want attribute.Set) metricdata.DataPoint[int64] {
	t.Helper()
	s, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("%s: data is %T, want Sum[int64]", m.Name, m.Data)
	}
	var found []metricdata.DataPoint[int64]
	for _, dp := range s.DataPoints {
		if dp.Attributes.Equals(&want) {
			found = append(found, dp)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s: %d datapoints match %v, want 1 (have %d total)", m.Name, len(found), want, len(s.DataPoints))
	}
	return found[0]
}

// gaugePoints returns every gauge datapoint of m keyed by its attribute set's
// canonical encoding.
func gaugeValue(t *testing.T, m metricdata.Metrics, want attribute.Set) int64 {
	t.Helper()
	g, ok := m.Data.(metricdata.Gauge[int64])
	if !ok {
		t.Fatalf("%s: data is %T, want Gauge[int64]", m.Name, m.Data)
	}
	var found []metricdata.DataPoint[int64]
	for _, dp := range g.DataPoints {
		if dp.Attributes.Equals(&want) {
			found = append(found, dp)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s: %d datapoints match %v, want 1 (have %d total)", m.Name, len(found), want, len(g.DataPoints))
	}
	return found[0].Value
}

// TestMetricsConsumerCleanCycle plays a successful cycle's event stream and
// asserts every event-derived instrument: values, units of seconds, and the
// exact attribute set each instrument is specified to carry.
func TestMetricsConsumerCleanCycle(t *testing.T) {
	emit, reader := newTestMetrics(t)

	emit(obs.Event{Time: at(0), Cycle: testCycle, Kind: obs.KindCycleStarted})

	emit(obs.Event{
		Time: at(1), Cycle: testCycle, Step: "BOOT", Kind: obs.KindStepEntered,
		StepInfo: &obs.StepEvent{State: "BOOT"},
	})
	emit(obs.Event{
		Time: at(2), Cycle: testCycle, Step: "BOOT", Kind: obs.KindActionStarted,
		Action: &obs.ActionEvent{Name: "dial"},
	})
	emit(obs.Event{
		Time: at(4), Cycle: testCycle, Step: "BOOT", Kind: obs.KindActionEnded,
		Action: &obs.ActionEvent{Name: "dial", Outcome: obs.OutcomeOK, Duration: 2 * time.Second},
	})
	emit(obs.Event{
		Time: at(6), Cycle: testCycle, Step: "BOOT", Kind: obs.KindStepLeft,
		StepInfo: &obs.StepEvent{State: "BOOT", Outcome: obs.OutcomeOK},
	})

	emit(obs.Event{
		Time: at(7), Cycle: testCycle, Step: "JOB", Kind: obs.KindStepEntered,
		StepInfo: &obs.StepEvent{State: "JOB"},
	})
	emit(obs.Event{
		Time: at(8), Cycle: testCycle, Step: "JOB", Kind: obs.KindJobStarted,
		Job: &obs.JobEvent{Name: "build"},
	})
	emit(obs.Event{
		Time: at(20), Cycle: testCycle, Step: "JOB", Kind: obs.KindJobEnded,
		Job: &obs.JobEvent{Name: "build", Outcome: obs.OutcomeOK},
	})
	emit(obs.Event{
		Time: at(21), Cycle: testCycle, Step: "JOB", Kind: obs.KindStepLeft,
		StepInfo: &obs.StepEvent{State: "JOB", Outcome: obs.OutcomeOK},
	})

	emit(obs.Event{
		Time: at(30), Cycle: testCycle, Kind: obs.KindCycleFinished,
		Finish: &obs.FinishEvent{Result: "success", Ending: "success"},
	})

	ms := collect(t, reader)

	pool := attribute.String("pool", testCycle.Pool)
	slot := attribute.String("slot", testCycle.Slot)

	cc := sumPoint(t, ms["runny.cycle.count"], attribute.NewSet(pool, slot,
		attribute.String("result", "success"), attribute.String("ending", "success")))
	if cc.Value != 1 {
		t.Errorf("cycle.count = %d, want 1", cc.Value)
	}

	cd := histPoint(t, ms["runny.cycle.duration"], attribute.NewSet(pool,
		attribute.String("result", "success")))
	if cd.Sum != 30 {
		t.Errorf("cycle.duration sum = %v s, want 30", cd.Sum)
	}

	sd := histPoint(t, ms["runny.step.duration"], attribute.NewSet(pool,
		attribute.String("step", "BOOT"), attribute.String("outcome", "ok")))
	if sd.Sum != 5 {
		t.Errorf("step.duration(BOOT) sum = %v s, want 5", sd.Sum)
	}

	jc := sumPoint(t, ms["runny.job.count"], attribute.NewSet(pool, slot,
		attribute.String("outcome", "ok")))
	if jc.Value != 1 {
		t.Errorf("job.count = %d, want 1", jc.Value)
	}
	jd := histPoint(t, ms["runny.job.duration"], attribute.NewSet(pool,
		attribute.String("outcome", "ok")))
	if jd.Sum != 12 {
		t.Errorf("job.duration sum = %v s, want 12", jd.Sum)
	}

	ad := histPoint(t, ms["runny.action.duration"], attribute.NewSet(
		attribute.String("step", "BOOT"), attribute.String("action", "dial"),
		attribute.String("outcome", "ok"),
	))
	if ad.Sum != 2 {
		t.Errorf("action.duration sum = %v s, want 2", ad.Sum)
	}

	for name, m := range ms {
		if _, ok := m.Data.(metricdata.Histogram[float64]); ok && m.Unit != "s" {
			t.Errorf("%s unit = %q, want \"s\"", name, m.Unit)
		}
	}
}

// TestMetricsConsumerFailureCycle asserts a failed step and a failed cycle
// land with their recorded outcome/result/ending labels.
func TestMetricsConsumerFailureCycle(t *testing.T) {
	emit, reader := newTestMetrics(t)

	emit(obs.Event{Time: at(0), Cycle: testCycle, Kind: obs.KindCycleStarted})
	emit(obs.Event{
		Time: at(1), Cycle: testCycle, Step: "BOOT", Kind: obs.KindStepEntered,
		StepInfo: &obs.StepEvent{State: "BOOT"},
	})
	emit(obs.Event{
		Time: at(4), Cycle: testCycle, Step: "BOOT", Kind: obs.KindStepLeft,
		StepInfo: &obs.StepEvent{State: "BOOT", Outcome: "deadline", Error: "boot deadline exceeded"},
	})
	emit(obs.Event{
		Time: at(5), Cycle: testCycle, Kind: obs.KindCycleFinished,
		Finish: &obs.FinishEvent{Result: "failure", Ending: "failure", FailureState: "BOOT"},
	})

	ms := collect(t, reader)
	pool := attribute.String("pool", testCycle.Pool)

	sd := histPoint(t, ms["runny.step.duration"], attribute.NewSet(pool,
		attribute.String("step", "BOOT"), attribute.String("outcome", "deadline")))
	if sd.Sum != 3 {
		t.Errorf("step.duration sum = %v s, want 3", sd.Sum)
	}

	cc := sumPoint(t, ms["runny.cycle.count"], attribute.NewSet(pool,
		attribute.String("slot", testCycle.Slot),
		attribute.String("result", "failure"), attribute.String("ending", "failure")))
	if cc.Value != 1 {
		t.Errorf("cycle.count = %d, want 1", cc.Value)
	}
}

// TestMetricsConsumerOrphanEvents: a StepLeft with no matching StepEntered
// and a JobEnded with no JobStarted must not fabricate a duration; JobEnded
// still counts (the job demonstrably ran).
func TestMetricsConsumerOrphanEvents(t *testing.T) {
	emit, reader := newTestMetrics(t)

	emit(obs.Event{
		Time: at(5), Cycle: testCycle, Step: "BOOT", Kind: obs.KindStepLeft,
		StepInfo: &obs.StepEvent{State: "BOOT", Outcome: obs.OutcomeOK},
	})
	emit(obs.Event{
		Time: at(6), Cycle: testCycle, Step: "JOB", Kind: obs.KindJobEnded,
		Job: &obs.JobEvent{Name: "build", Outcome: obs.OutcomeOK},
	})

	ms := collect(t, reader)

	if m, ok := ms["runny.step.duration"]; ok {
		if h, ok := m.Data.(metricdata.Histogram[float64]); ok && len(h.DataPoints) > 0 {
			t.Errorf("step.duration has %d datapoints for an orphan StepLeft, want 0", len(h.DataPoints))
		}
	}
	if m, ok := ms["runny.job.duration"]; ok {
		if h, ok := m.Data.(metricdata.Histogram[float64]); ok && len(h.DataPoints) > 0 {
			t.Errorf("job.duration has %d datapoints for an orphan JobEnded, want 0", len(h.DataPoints))
		}
	}
	jc := sumPoint(t, ms["runny.job.count"], attribute.NewSet(
		attribute.String("pool", testCycle.Pool), attribute.String("slot", testCycle.Slot),
		attribute.String("outcome", "ok"),
	))
	if jc.Value != 1 {
		t.Errorf("job.count = %d, want 1 (orphan JobEnded still counts)", jc.Value)
	}
}

// TestMetricsConsumerTrackingCleanup: CycleFinished must drop the cycle's
// open-step tracking so a long-lived consumer doesn't accrete state.
func TestMetricsConsumerTrackingCleanup(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	m := &metricsConsumer{open: map[cycleKey]*openCycle{}}
	if err := m.instruments(mp.Meter("test")); err != nil {
		t.Fatalf("instruments: %v", err)
	}

	m.emit(obs.Event{
		Time: at(1), Cycle: testCycle, Step: "BOOT", Kind: obs.KindStepEntered,
		StepInfo: &obs.StepEvent{State: "BOOT"},
	})
	if len(m.open) != 1 {
		t.Fatalf("open cycles = %d after StepEntered, want 1", len(m.open))
	}
	m.emit(obs.Event{
		Time: at(2), Cycle: testCycle, Kind: obs.KindCycleFinished,
		Finish: &obs.FinishEvent{Result: "failure", Ending: "shutdown"},
	})
	if len(m.open) != 0 {
		t.Errorf("open cycles = %d after CycleFinished, want 0", len(m.open))
	}
}

// TestRegisterGauges polls a faked three-slot fleet and asserts the full 0/1
// state matrix, per-slot scalars, and the home-dir disk gauge.
func TestRegisterGauges(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	states := []string{"BACKOFF", "LISTENING", "JOB"}
	entered := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	snaps := []SlotSnapshot{
		{Pool: "macos-arm", Slot: "s0", State: "JOB", StateEntered: entered},
		{Pool: "macos-arm", Slot: "s1", State: "LISTENING", StateEntered: entered, ConsecutiveFailures: 3, Paused: true},
		{Pool: "macos-x64", Slot: "s2", State: "BACKOFF", StateEntered: entered, Wedged: true},
	}
	if err := RegisterGauges(mp.Meter("test"), func() []SlotSnapshot { return snaps }, states, t.TempDir()); err != nil {
		t.Fatalf("RegisterGauges: %v", err)
	}

	ms := collect(t, reader)

	set := func(pool, slot string, extra ...attribute.KeyValue) attribute.Set {
		return attribute.NewSet(append([]attribute.KeyValue{
			attribute.String("pool", pool), attribute.String("slot", slot),
		}, extra...)...)
	}

	// The state matrix: every (slot, state) pair reports, exactly one 1 per slot.
	for _, snap := range snaps {
		for _, st := range states {
			want := int64(0)
			if st == snap.State {
				want = 1
			}
			got := gaugeValue(t, ms["runny.slot.state"], set(snap.Pool, snap.Slot, attribute.String("state", st)))
			if got != want {
				t.Errorf("slot.state{%s,%s} = %d, want %d", snap.Slot, st, got, want)
			}
		}
	}

	if got := gaugeValue(t, ms["runny.slot.state_entered_time"], set("macos-arm", "s0")); got != entered.Unix() {
		t.Errorf("state_entered_time = %d, want %d", got, entered.Unix())
	}
	if got := gaugeValue(t, ms["runny.slot.consecutive_failures"], set("macos-arm", "s1")); got != 3 {
		t.Errorf("consecutive_failures(s1) = %d, want 3", got)
	}
	if got := gaugeValue(t, ms["runny.slot.paused"], set("macos-arm", "s1")); got != 1 {
		t.Errorf("paused(s1) = %d, want 1", got)
	}
	if got := gaugeValue(t, ms["runny.slot.paused"], set("macos-arm", "s0")); got != 0 {
		t.Errorf("paused(s0) = %d, want 0", got)
	}
	if got := gaugeValue(t, ms["runny.slot.wedged"], set("macos-x64", "s2")); got != 1 {
		t.Errorf("wedged(s2) = %d, want 1", got)
	}
	if got := gaugeValue(t, ms["runny.home.disk.free_bytes"], attribute.NewSet()); got <= 0 {
		t.Errorf("disk.free_bytes = %d, want > 0", got)
	}
	if m := ms["runny.home.disk.free_bytes"]; m.Unit != "By" {
		t.Errorf("disk.free_bytes unit = %q, want \"By\"", m.Unit)
	}
}

// TestRegisterGaugesPreTransitionSlot: a slot that hasn't transitioned yet
// (zero State/StateEntered) reports an all-zero state matrix and no
// state_entered_time point — never a fabricated epoch-negative timestamp.
func TestRegisterGaugesPreTransitionSlot(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	states := []string{"BACKOFF", "JOB"}
	snaps := []SlotSnapshot{{Pool: "macos-arm", Slot: "s0"}}
	if err := RegisterGauges(mp.Meter("test"), func() []SlotSnapshot { return snaps }, states, t.TempDir()); err != nil {
		t.Fatalf("RegisterGauges: %v", err)
	}

	ms := collect(t, reader)
	for _, st := range states {
		got := gaugeValue(t, ms["runny.slot.state"], attribute.NewSet(
			attribute.String("pool", "macos-arm"), attribute.String("slot", "s0"),
			attribute.String("state", st),
		))
		if got != 0 {
			t.Errorf("slot.state{%s} = %d, want 0", st, got)
		}
	}
	if m, ok := ms["runny.slot.state_entered_time"]; ok {
		if g, ok := m.Data.(metricdata.Gauge[int64]); ok && len(g.DataPoints) > 0 {
			t.Errorf("state_entered_time has %d datapoints for a pre-transition slot, want 0", len(g.DataPoints))
		}
	}
}

// TestRegisterGaugesDiskErrorSurfaces: an unreadable home path must surface
// as a Collect error (routed to the OTEL error handler in production), never
// a silent skip or a fake zero.
func TestRegisterGaugesDiskErrorSurfaces(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	if err := RegisterGauges(mp.Meter("test"), func() []SlotSnapshot { return nil }, nil,
		"/nonexistent/runny-home"); err != nil {
		t.Fatalf("RegisterGauges: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err == nil {
		t.Error("Collect returned nil error for an unreadable home path, want statfs error")
	}

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == "runny.home.disk.free_bytes" {
				if g, ok := m.Data.(metricdata.Gauge[int64]); ok && len(g.DataPoints) > 0 {
					t.Errorf("disk.free_bytes has %d datapoints despite statfs error, want 0", len(g.DataPoints))
				}
			}
		}
	}
}
