package telemetry

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/bojanrajkovic/runny/internal/diskfree"
	"github.com/bojanrajkovic/runny/internal/obs"
)

// NewMetricsConsumer returns an obs.Emitter that folds the event stream into
// the cycle/step/job/action instruments. Same sharing contract as
// NewTraceConsumer: one instance serves every slot, with internal locking on
// the open-cycle tracking (instrument Add/Record are already
// goroutine-safe). Durations are computed from event timestamps, never a
// fresh clock read, so a replayed stream produces the same numbers the live
// one did. Every label value comes from a closed set — states, outcomes,
// action names, config'd pools and slots — never a guest-controlled string.
func NewMetricsConsumer(meter metric.Meter) (obs.Emitter, error) {
	m := &metricsConsumer{open: map[cycleKey]*openCycle{}}
	if err := m.instruments(meter); err != nil {
		return nil, err
	}
	return m.emit, nil
}

// openCycle remembers the start timestamps StepLeft/JobEnded need but don't
// carry. It exists per in-flight cycle and is dropped at CycleFinished.
type openCycle struct {
	stepEntered map[string]time.Time
	jobStarted  time.Time
}

type metricsConsumer struct {
	// cycleCount increments once per finished cycle, at CycleFinished.
	// `result` is the recorded cycle outcome (success/failure) and `ending`
	// the persisted classification (success/failure/wedge/shutdown/...), so
	// benign endings are distinguishable from health failures.
	cycleCount metric.Int64Counter
	// cycleDuration is CycleFinished minus the cycle's recorded start.
	cycleDuration metric.Float64Histogram
	// stepDuration is one point per StepLeft with a matching StepEntered;
	// `outcome` passes through the FSM's vocabulary (ok/error/warn/deadline).
	stepDuration metric.Float64Histogram
	// jobCount/jobDuration fire at JobEnded — a cycle hosts at most one job,
	// so job throughput is countable without deduplication.
	jobCount    metric.Int64Counter
	jobDuration metric.Float64Histogram
	// actionDuration is one point per ActionEnded, using the duration the
	// obs.Action wrapper measured; `action` names come from the closed const
	// set in internal/obs.
	actionDuration metric.Float64Histogram

	mu   sync.Mutex
	open map[cycleKey]*openCycle
}

// openFor returns key's tracking entry, creating it on first use. Callers
// must hold m.mu.
func (m *metricsConsumer) openFor(key cycleKey) *openCycle {
	oc := m.open[key]
	if oc == nil {
		oc = &openCycle{stepEntered: map[string]time.Time{}}
		m.open[key] = oc
	}
	return oc
}

func (m *metricsConsumer) instruments(meter metric.Meter) error {
	var errs []error
	var err error
	m.cycleCount, err = meter.Int64Counter("runny.cycle.count",
		metric.WithUnit("{cycle}"),
		metric.WithDescription("Finished runner cycles, by recorded result and persisted ending classification."))
	errs = append(errs, err)
	m.cycleDuration, err = meter.Float64Histogram("runny.cycle.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Wall-clock duration of a finished cycle, start to CycleFinished."))
	errs = append(errs, err)
	m.stepDuration, err = meter.Float64Histogram("runny.step.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Duration of one FSM step, StepEntered to StepLeft, by outcome."))
	errs = append(errs, err)
	m.jobCount, err = meter.Int64Counter("runny.job.count",
		metric.WithUnit("{job}"),
		metric.WithDescription("GitHub Actions jobs finished on a slot's runner, by outcome."))
	errs = append(errs, err)
	m.jobDuration, err = meter.Float64Histogram("runny.job.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Duration of one job, JobStarted to JobEnded."))
	errs = append(errs, err)
	m.actionDuration, err = meter.Float64Histogram("runny.action.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Duration of one obs.Action sub-step within an FSM step."))
	errs = append(errs, err)
	return errors.Join(errs...)
}

func (m *metricsConsumer) emit(e obs.Event) {
	key := cycleKey{e.Cycle.Slot, e.Cycle.CycleID}
	pool := attribute.String("pool", e.Cycle.Pool)
	slot := attribute.String("slot", e.Cycle.Slot)
	ctx := context.Background()

	switch e.Kind {
	case obs.KindStepEntered:
		m.mu.Lock()
		m.openFor(key).stepEntered[e.Step] = e.Time
		m.mu.Unlock()

	case obs.KindStepLeft:
		m.mu.Lock()
		var entered time.Time
		var ok bool
		if oc := m.open[key]; oc != nil {
			entered, ok = oc.stepEntered[e.Step]
			delete(oc.stepEntered, e.Step)
		}
		m.mu.Unlock()
		// No matching StepEntered (a stray event) → no fabricated duration.
		if ok && e.StepInfo != nil {
			m.stepDuration.Record(ctx, e.Time.Sub(entered).Seconds(), metric.WithAttributes(
				pool, attribute.String("step", e.Step),
				attribute.String("outcome", string(e.StepInfo.Outcome)),
			))
		}

	case obs.KindJobStarted:
		m.mu.Lock()
		m.openFor(key).jobStarted = e.Time
		m.mu.Unlock()

	case obs.KindJobEnded:
		if e.Job == nil {
			return
		}
		m.mu.Lock()
		var started time.Time
		if oc := m.open[key]; oc != nil {
			started = oc.jobStarted
		}
		m.mu.Unlock()
		outcome := attribute.String("outcome", string(e.Job.Outcome))
		// The job demonstrably ran even when its start was never seen: count
		// unconditionally, record a duration only against a real start.
		m.jobCount.Add(ctx, 1, metric.WithAttributes(pool, slot, outcome))
		if !started.IsZero() {
			m.jobDuration.Record(ctx, e.Time.Sub(started).Seconds(), metric.WithAttributes(pool, outcome))
		}

	case obs.KindActionEnded:
		if e.Action == nil {
			return
		}
		m.actionDuration.Record(ctx, e.Action.Duration.Seconds(), metric.WithAttributes(
			attribute.String("step", e.Step),
			attribute.String("action", e.Action.Name),
			attribute.String("outcome", string(e.Action.Outcome)),
		))

	case obs.KindCycleFinished:
		m.mu.Lock()
		delete(m.open, key)
		m.mu.Unlock()
		if e.Finish == nil {
			return
		}
		result := attribute.String("result", e.Finish.Result)
		ending := attribute.String("ending", e.Finish.Ending)
		m.cycleCount.Add(ctx, 1, metric.WithAttributes(pool, slot, result, ending))
		// The duration carries `ending` too: an operator recycle or a daemon
		// shutdown truncates a cycle and records it result=failure, and
		// without the ending label those benign truncations would pollute
		// the real failure-duration distribution unfixably. The zero-Started
		// guard is the same no-fabricated-duration rule steps and jobs
		// follow.
		if !e.Cycle.Started.IsZero() {
			m.cycleDuration.Record(ctx, e.Time.Sub(e.Cycle.Started).Seconds(),
				metric.WithAttributes(pool, result, ending))
		}
	}
}

// SlotSnapshot is the neutral per-slot view the gauge callback polls —
// telemetry stays free of an internal/statemachine import, and tests fake a
// fleet with plain values. cmd/runnyd adapts each slot's Status() into one
// of these.
type SlotSnapshot struct {
	Pool, Slot, State   string
	StateEntered        time.Time
	ConsecutiveFailures uint32
	Wedged, Paused      bool
}

// RegisterGauges installs the status-polled observable gauges: slot state
// (a 0/1 matrix over the full closed state list, so a state change reports 0
// on the old series instead of going stale), state-entered time, failure
// streak, wedged/paused flags, and free bytes on the runny home filesystem.
// Collection cost is one poll() (a Status() snapshot per slot) plus one
// statfs — no FSM involvement, no other I/O. A statfs failure is reported to
// the OTEL error handler and the disk gauge is omitted for that collection —
// never a fake zero, and never a callback error: the SDK skips exporting the
// ENTIRE collection when any callback errors, so returning it would black
// out every runny metric during exactly the disk incident the slot gauges
// need to narrate.
func RegisterGauges(meter metric.Meter, poll func() []SlotSnapshot, states []string, homePath string) error {
	var errs []error
	state, err := meter.Int64ObservableGauge("runny.slot.state",
		metric.WithDescription("1 when the slot is in the labeled FSM state, else 0; one series per (pool, slot, state)."))
	errs = append(errs, err)
	entered, err := meter.Int64ObservableGauge("runny.slot.state_entered_time",
		metric.WithUnit("s"),
		metric.WithDescription("Unix seconds the slot entered its current state; time-in-state = now minus this."))
	errs = append(errs, err)
	failures, err := meter.Int64ObservableGauge("runny.slot.consecutive_failures",
		metric.WithDescription("Current consecutive-failure streak feeding the slot's backoff."))
	errs = append(errs, err)
	wedged, err := meter.Int64ObservableGauge("runny.slot.wedged",
		metric.WithDescription("1 when the slot's guest survived force-stop and the slot is parked until daemon restart."))
	errs = append(errs, err)
	paused, err := meter.Int64ObservableGauge("runny.slot.paused",
		metric.WithDescription("1 when the slot is operator-paused."))
	errs = append(errs, err)
	disk, err := meter.Int64ObservableGauge("runny.home.disk.free_bytes",
		metric.WithUnit("By"),
		metric.WithDescription("Available bytes on the runny home filesystem, as ENSURE_IMAGE's headroom checks see it."))
	errs = append(errs, err)
	if err := errors.Join(errs...); err != nil {
		return err
	}

	b2i := func(b bool) int64 {
		if b {
			return 1
		}
		return 0
	}

	_, err = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		for _, s := range poll() {
			slotAttrs := metric.WithAttributes(
				attribute.String("pool", s.Pool), attribute.String("slot", s.Slot),
			)
			for _, st := range states {
				o.ObserveInt64(state, b2i(st == s.State), metric.WithAttributes(
					attribute.String("pool", s.Pool), attribute.String("slot", s.Slot),
					attribute.String("state", st),
				))
			}
			// A slot that hasn't transitioned yet has a zero StateEntered;
			// skip rather than report a nonsense negative epoch.
			if !s.StateEntered.IsZero() {
				o.ObserveInt64(entered, s.StateEntered.Unix(), slotAttrs)
			}
			o.ObserveInt64(failures, int64(s.ConsecutiveFailures), slotAttrs)
			o.ObserveInt64(wedged, b2i(s.Wedged), slotAttrs)
			o.ObserveInt64(paused, b2i(s.Paused), slotAttrs)
		}
		free, err := diskfree.AvailableBytes(homePath)
		if err != nil {
			otel.Handle(fmt.Errorf("telemetry: disk free on %s: %w", homePath, err))
			return nil
		}
		o.ObserveInt64(disk, int64(free))
		return nil
	}, state, entered, failures, wedged, paused, disk)
	return err
}
