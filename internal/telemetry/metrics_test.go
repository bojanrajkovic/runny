package telemetry

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/bojanrajkovic/runny/internal/obs"
)

func newTestMeter(t *testing.T) (metric.Meter, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	return mp.Meter("test"), reader
}

func newTestMetrics(t *testing.T) (*metricsConsumer, *sdkmetric.ManualReader) {
	t.Helper()
	meter, reader := newTestMeter(t)
	m := &metricsConsumer{}
	if err := m.instruments(meter); err != nil {
		t.Fatalf("instruments: %v", err)
	}
	return m, reader
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

// onePoint finds the single datapoint whose attribute set matches want
// exactly, failing the test when it's absent or duplicated.
func onePoint[DP any](t *testing.T, name string, dps []DP, attrsOf func(DP) attribute.Set, want attribute.Set) DP {
	t.Helper()
	var found []DP
	for _, dp := range dps {
		if a := attrsOf(dp); a.Equals(&want) {
			found = append(found, dp)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s: %d datapoints match %v, want 1 (have %d total)", name, len(found), want, len(dps))
	}
	return found[0]
}

// histPoint, sumPoint, and gaugeValue are onePoint over the three datapoint
// shapes the instruments under test produce.
func histPoint(t *testing.T, m metricdata.Metrics, want attribute.Set) metricdata.HistogramDataPoint[float64] {
	t.Helper()
	h, ok := m.Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("%s: data is %T, want Histogram[float64]", m.Name, m.Data)
	}
	return onePoint(t, m.Name, h.DataPoints,
		func(dp metricdata.HistogramDataPoint[float64]) attribute.Set { return dp.Attributes }, want)
}

func sumPoint(t *testing.T, m metricdata.Metrics, want attribute.Set) metricdata.DataPoint[int64] {
	t.Helper()
	s, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("%s: data is %T, want Sum[int64]", m.Name, m.Data)
	}
	return onePoint(t, m.Name, s.DataPoints,
		func(dp metricdata.DataPoint[int64]) attribute.Set { return dp.Attributes }, want)
}

func gaugeValue(t *testing.T, m metricdata.Metrics, want attribute.Set) int64 {
	t.Helper()
	g, ok := m.Data.(metricdata.Gauge[int64])
	if !ok {
		t.Fatalf("%s: data is %T, want Gauge[int64]", m.Name, m.Data)
	}
	return onePoint(t, m.Name, g.DataPoints,
		func(dp metricdata.DataPoint[int64]) attribute.Set { return dp.Attributes }, want).Value
}

// TestMetricsConsumerCleanCycle plays a successful cycle's event stream and
// asserts every event-derived instrument: values, units of seconds, and the
// exact attribute set each instrument is specified to carry.
func TestMetricsConsumerCleanCycle(t *testing.T) {
	m, reader := newTestMetrics(t)

	m.emit(obs.Event{Time: at(0), Cycle: testCycle, Kind: obs.KindCycleStarted})

	m.emit(obs.Event{
		Time: at(1), Cycle: testCycle, Step: "BOOT", Kind: obs.KindStepEntered,
		StepInfo: &obs.StepEvent{State: "BOOT"},
	})
	m.emit(obs.Event{
		Time: at(2), Cycle: testCycle, Step: "BOOT", Kind: obs.KindActionStarted,
		Action: &obs.ActionEvent{Name: "dial"},
	})
	m.emit(obs.Event{
		Time: at(4), Cycle: testCycle, Step: "BOOT", Kind: obs.KindActionEnded,
		Action: &obs.ActionEvent{Name: "dial", Outcome: obs.OutcomeOK, Duration: 2 * time.Second},
	})
	m.emit(obs.Event{
		Time: at(6), Cycle: testCycle, Step: "BOOT", Kind: obs.KindStepLeft,
		StepInfo: &obs.StepEvent{State: "BOOT", Outcome: obs.OutcomeOK, Duration: 5 * time.Second},
	})

	m.emit(obs.Event{
		Time: at(7), Cycle: testCycle, Step: "JOB", Kind: obs.KindStepEntered,
		StepInfo: &obs.StepEvent{State: "JOB"},
	})
	m.emit(obs.Event{
		Time: at(8), Cycle: testCycle, Step: "JOB", Kind: obs.KindJobStarted,
		Job: &obs.JobEvent{Name: "build"},
	})
	m.emit(obs.Event{
		Time: at(20), Cycle: testCycle, Step: "JOB", Kind: obs.KindJobEnded,
		Job: &obs.JobEvent{Name: "build", Outcome: obs.OutcomeOK, Duration: 12 * time.Second},
	})
	m.emit(obs.Event{
		Time: at(21), Cycle: testCycle, Step: "JOB", Kind: obs.KindStepLeft,
		StepInfo: &obs.StepEvent{State: "JOB", Outcome: obs.OutcomeOK, Duration: 14 * time.Second},
	})

	m.emit(obs.Event{
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
		attribute.String("result", "success"), attribute.String("ending", "success")))
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
	m, reader := newTestMetrics(t)

	m.emit(obs.Event{Time: at(0), Cycle: testCycle, Kind: obs.KindCycleStarted})
	m.emit(obs.Event{
		Time: at(1), Cycle: testCycle, Step: "BOOT", Kind: obs.KindStepEntered,
		StepInfo: &obs.StepEvent{State: "BOOT"},
	})
	m.emit(obs.Event{
		Time: at(4), Cycle: testCycle, Step: "BOOT", Kind: obs.KindStepLeft,
		StepInfo: &obs.StepEvent{State: "BOOT", Outcome: "deadline", Error: "boot deadline exceeded", Duration: 3 * time.Second},
	})
	m.emit(obs.Event{
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

	// The duration histogram carries ending too, so a shutdown-truncated
	// cycle (result=failure, ending=shutdown) is excludable from the real
	// failure-duration distribution.
	cd := histPoint(t, ms["runny.cycle.duration"], attribute.NewSet(pool,
		attribute.String("result", "failure"), attribute.String("ending", "failure")))
	if cd.Sum != 5 {
		t.Errorf("cycle.duration sum = %v s, want 5", cd.Sum)
	}
}

// TestMetricsConsumerOrphanEvents: a StepLeft or JobEnded carrying no
// Duration (a stray event, or one from an older daemon build that predates
// this field) must not fabricate one; JobEnded still counts (the job
// demonstrably ran).
func TestMetricsConsumerOrphanEvents(t *testing.T) {
	m, reader := newTestMetrics(t)

	m.emit(obs.Event{
		Time: at(5), Cycle: testCycle, Step: "BOOT", Kind: obs.KindStepLeft,
		StepInfo: &obs.StepEvent{State: "BOOT", Outcome: obs.OutcomeOK},
	})
	m.emit(obs.Event{
		Time: at(6), Cycle: testCycle, Step: "JOB", Kind: obs.KindJobEnded,
		Job: &obs.JobEvent{Name: "build", Outcome: obs.OutcomeOK},
	})

	ms := collect(t, reader)

	if m, ok := ms["runny.step.duration"]; ok {
		if h, ok := m.Data.(metricdata.Histogram[float64]); ok && len(h.DataPoints) > 0 {
			t.Errorf("step.duration has %d datapoints for a zero-Duration StepLeft, want 0", len(h.DataPoints))
		}
	}
	if m, ok := ms["runny.job.duration"]; ok {
		if h, ok := m.Data.(metricdata.Histogram[float64]); ok && len(h.DataPoints) > 0 {
			t.Errorf("job.duration has %d datapoints for a zero-Duration JobEnded, want 0", len(h.DataPoints))
		}
	}
	jc := sumPoint(t, ms["runny.job.count"], attribute.NewSet(
		attribute.String("pool", testCycle.Pool), attribute.String("slot", testCycle.Slot),
		attribute.String("outcome", "ok"),
	))
	if jc.Value != 1 {
		t.Errorf("job.count = %d, want 1 (a zero-Duration JobEnded still counts)", jc.Value)
	}
}

// KindPullStarted carries no payload consumers record from — it exists so
// the trace side has a root to open — so this consumer's switch has no case
// for it at all; feeding one through must be a true no-op.
func TestMetricsConsumerIgnoresPullStarted(t *testing.T) {
	m, reader := newTestMetrics(t)
	m.emit(obs.Event{Kind: obs.KindPullStarted, Pull: &obs.PullRef{ID: "p1"}})
	ms := collect(t, reader)
	if got, ok := ms["runny.image.pull.duration"]; ok {
		if h, ok := got.Data.(metricdata.Histogram[float64]); ok && len(h.DataPoints) > 0 {
			t.Errorf("pull.duration has %d datapoints for KindPullStarted, want 0", len(h.DataPoints))
		}
	}
}

// The pull/tarball instruments, folded straight out of the event stream:
// KindPullFinished feeds runny.image.pull.duration/bytes, KindTarballDone
// feeds runny.runner_tarball.download.duration. Names, units, and
// descriptions are pinned exactly — Grafana/Tempo dashboards query these
// names. Neither carries a Cycle, so neither carries a pool/slot label; the
// exact attribute-set match in histPoint's onePoint already proves that (a
// stray pool/slot label would make the lookup fail to find a match).
func TestMetricsConsumerPullAndTarballFold(t *testing.T) {
	m, reader := newTestMetrics(t)

	m.emit(obs.Event{Kind: obs.KindPullFinished, Pull: &obs.PullRef{ID: "p1"}, PullInfo: &obs.PullEvent{
		Outcome: obs.OutcomeOK, Duration: 90 * time.Second, Bytes: 5 << 30,
	}})
	m.emit(obs.Event{Kind: obs.KindTarballDone, Cycle: testCycle, Tarball: &obs.TarballEvent{
		Outcome: obs.OutcomeError, Duration: 3 * time.Second,
	}})

	ms := collect(t, reader)
	okSet := attribute.NewSet(attribute.String("outcome", "ok"))
	errSet := attribute.NewSet(attribute.String("outcome", "error"))

	dur := ms["runny.image.pull.duration"]
	if dur.Unit != "s" {
		t.Errorf("pull.duration unit = %q, want s", dur.Unit)
	}
	if want := "Wall-clock lifetime of one underlying image pull, including disk holds and re-attempts, recorded once at its terminal outcome regardless of how many slots shared it."; dur.Description != want {
		t.Errorf("pull.duration description = %q, want %q", dur.Description, want)
	}
	if p := histPoint(t, dur, okSet); p.Sum != 90 {
		t.Errorf("pull.duration sum = %v, want 90", p.Sum)
	}

	bytes := ms["runny.image.pull.bytes"]
	if bytes.Unit != "By" {
		t.Errorf("pull.bytes unit = %q, want By", bytes.Unit)
	}
	if want := "Bytes transferred by one underlying image pull, cumulative across its attempts (can exceed the image size on retry)."; bytes.Description != want {
		t.Errorf("pull.bytes description = %q, want %q", bytes.Description, want)
	}
	if p := histPoint(t, bytes, okSet); p.Sum != float64(int64(5<<30)) {
		t.Errorf("pull.bytes sum = %v, want 5 GiB", p.Sum)
	}

	tb := ms["runny.runner_tarball.download.duration"]
	if tb.Unit != "s" {
		t.Errorf("tarball duration unit = %q, want s", tb.Unit)
	}
	if want := "Duration of one actual runner-tarball download — cache hits and slots that waited out a peer's download record nothing."; tb.Description != want {
		t.Errorf("tarball duration description = %q, want %q", tb.Description, want)
	}
	if p := histPoint(t, tb, errSet); p.Sum != 3 {
		t.Errorf("tarball duration sum = %v, want 3", p.Sum)
	}
}

// A nil payload (a stray or malformed event) must not panic either fold.
func TestMetricsConsumerNilPullTarballPayloadsAreNoop(t *testing.T) {
	m, _ := newTestMetrics(t)
	m.emit(obs.Event{Kind: obs.KindPullFinished, Pull: &obs.PullRef{ID: "p1"}})
	m.emit(obs.Event{Kind: obs.KindTarballDone, Cycle: testCycle})
}

// TestRegisterGauges polls a faked three-slot fleet and asserts the full 0/1
// state matrix, per-slot scalars, and the home-dir disk gauge.
func TestRegisterGauges(t *testing.T) {
	meter, reader := newTestMeter(t)

	states := []string{"BACKOFF", "LISTENING", "JOB"}
	entered := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	snaps := []SlotSnapshot{
		{Pool: "macos-arm", Slot: "s0", State: "JOB", StateEntered: entered},
		{Pool: "macos-arm", Slot: "s1", State: "LISTENING", StateEntered: entered, ConsecutiveFailures: 3, Paused: true},
		{Pool: "macos-x64", Slot: "s2", State: "BACKOFF", StateEntered: entered, Wedged: true},
	}
	if err := RegisterGauges(meter, func() []SlotSnapshot { return snaps }, states, t.TempDir()); err != nil {
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
	meter, reader := newTestMeter(t)

	states := []string{"BACKOFF", "JOB"}
	snaps := []SlotSnapshot{{Pool: "macos-arm", Slot: "s0"}}
	if err := RegisterGauges(meter, func() []SlotSnapshot { return snaps }, states, t.TempDir()); err != nil {
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

// TestRegisterGaugesDiskErrorBlastRadius: an unreadable home path must not
// take the rest of the collection down with it. The SDK skips exporting the
// whole interval when a callback returns an error, so the callback must
// report the statfs failure out of band (the OTEL error handler) and keep
// observing: slot gauges present, no fabricated disk point, Collect clean.
func TestRegisterGaugesDiskErrorBlastRadius(t *testing.T) {
	meter, reader := newTestMeter(t)

	var handled []error
	prev := otel.GetErrorHandler()
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) { handled = append(handled, err) }))
	t.Cleanup(func() { otel.SetErrorHandler(prev) })

	snaps := []SlotSnapshot{{Pool: "macos-arm", Slot: "s0", State: "JOB", StateEntered: at(0)}}
	if err := RegisterGauges(meter, func() []SlotSnapshot { return snaps }, []string{"JOB"},
		"/nonexistent/runny-home"); err != nil {
		t.Fatalf("RegisterGauges: %v", err)
	}

	// collect fails the test on a Collect error, which is the point: the
	// statfs failure must not abort the collection.
	ms := collect(t, reader)

	if got := gaugeValue(t, ms["runny.slot.state"], attribute.NewSet(
		attribute.String("pool", "macos-arm"), attribute.String("slot", "s0"),
		attribute.String("state", "JOB"),
	)); got != 1 {
		t.Errorf("slot.state = %d despite disk error, want 1 (slot gauges must survive)", got)
	}
	if m, ok := ms["runny.home.disk.free_bytes"]; ok {
		if g, ok := m.Data.(metricdata.Gauge[int64]); ok && len(g.DataPoints) > 0 {
			t.Errorf("disk.free_bytes has %d datapoints despite statfs error, want 0", len(g.DataPoints))
		}
	}
	if len(handled) == 0 {
		t.Error("statfs error was not reported to the OTEL error handler; loss must never be silent")
	}
}
