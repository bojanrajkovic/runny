package telemetry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/bojanrajkovic/runny/internal/diskfree"
	"github.com/bojanrajkovic/runny/internal/images"
	"github.com/bojanrajkovic/runny/internal/obs"
)

// NewMetricsConsumer returns an obs.Emitter that folds the event stream into
// the cycle/step/job/action instruments — a pure fold with no per-cycle
// state of its own: StepLeft and JobEnded carry their own Duration (the FSM
// stamps it from the StateRecord/JobInfo it already holds), so there is
// nothing to track between events. Same sharing contract as
// NewTraceConsumer: one instance serves every slot; instrument Add/Record
// are already goroutine-safe, so no locking is needed here either. Every
// label value comes from a closed set — states, outcomes, action names,
// config'd pools and slots — never a guest-controlled string.
func NewMetricsConsumer(meter metric.Meter) (obs.Emitter, error) {
	m := &metricsConsumer{}
	if err := m.instruments(meter); err != nil {
		return nil, err
	}
	return m.emit, nil
}

type metricsConsumer struct {
	// cycleCount increments once per finished cycle, at CycleFinished.
	// `result` is the recorded cycle outcome (success/failure) and `ending`
	// the persisted classification (success/failure/wedge/shutdown/...), so
	// benign endings are distinguishable from health failures.
	cycleCount metric.Int64Counter
	// cycleDuration is CycleFinished minus the cycle's recorded start.
	cycleDuration metric.Float64Histogram
	// stepDuration is one point per StepLeft; `outcome` passes through the
	// FSM's vocabulary (ok/error/warn/deadline).
	stepDuration metric.Float64Histogram
	// jobCount/jobDuration fire at JobEnded — a cycle hosts at most one job,
	// so job throughput is countable without deduplication.
	jobCount    metric.Int64Counter
	jobDuration metric.Float64Histogram
	// actionDuration is one point per ActionEnded, using the duration the
	// obs.Action wrapper measured; `action` names come from the closed const
	// set in internal/obs.
	actionDuration metric.Float64Histogram
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
	pool := attribute.String("pool", e.Cycle.Pool)
	slot := attribute.String("slot", e.Cycle.Slot)
	ctx := context.Background()

	switch e.Kind {
	case obs.KindStepLeft:
		// A zero Duration (a stray or legacy event) skips the histogram —
		// the same no-fabricated-duration rule the entered-lookup miss used
		// to produce.
		if e.StepInfo != nil && e.StepInfo.Duration > 0 {
			m.stepDuration.Record(ctx, e.StepInfo.Duration.Seconds(), metric.WithAttributes(
				pool, attribute.String("step", e.Step),
				attribute.String("outcome", string(e.StepInfo.Outcome)),
			))
		}

	case obs.KindJobEnded:
		if e.Job == nil {
			return
		}
		outcome := attribute.String("outcome", string(e.Job.Outcome))
		// The job demonstrably ran even when its start was never seen: count
		// unconditionally, record a duration only when one was stamped.
		m.jobCount.Add(ctx, 1, metric.WithAttributes(pool, slot, outcome))
		if e.Job.Duration > 0 {
			m.jobDuration.Record(ctx, e.Job.Duration.Seconds(), metric.WithAttributes(pool, outcome))
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

// NewEnsurerMetrics builds the ensurer-scope instruments and returns them
// behind the images.Metrics seam — the image ensurer records pull and
// tarball-download outcomes through it without importing any OTEL type.
// These are per-underlying-work truths, not per-cycle ones (a shared pull
// serves many subscribing slots and belongs to none of them), which is why
// they record here instead of folding out of the cycle event stream.
// `outcome` is the closed ok/error vocabulary; durations are seconds, bytes
// are bytes, and the exponential-histogram view Setup installs applies to
// all three.
func NewEnsurerMetrics(meter metric.Meter) (*images.Metrics, error) {
	var errs []error
	pullDur, err := meter.Float64Histogram("runny.image.pull.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Wall-clock lifetime of one underlying image pull, including disk holds and re-attempts, recorded once at its terminal outcome regardless of how many slots shared it."))
	errs = append(errs, err)
	pullBytes, err := meter.Float64Histogram("runny.image.pull.bytes",
		metric.WithUnit("By"),
		metric.WithDescription("Bytes transferred by one underlying image pull, cumulative across its attempts (can exceed the image size on retry)."))
	errs = append(errs, err)
	tarballDur, err := meter.Float64Histogram("runny.runner_tarball.download.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Duration of one actual runner-tarball download — cache hits and slots that waited out a peer's download record nothing."))
	errs = append(errs, err)
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return &images.Metrics{
		PullDone: func(outcome string, d time.Duration, bytes int64) {
			attrs := metric.WithAttributes(attribute.String("outcome", outcome))
			pullDur.Record(context.Background(), d.Seconds(), attrs)
			pullBytes.Record(context.Background(), float64(bytes), attrs)
		},
		TarballDownloadDone: func(outcome string, d time.Duration) {
			tarballDur.Record(context.Background(), d.Seconds(),
				metric.WithAttributes(attribute.String("outcome", outcome)))
		},
	}, nil
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
