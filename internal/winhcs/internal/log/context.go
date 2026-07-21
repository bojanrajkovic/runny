package log

import (
	"context"
	"fmt"
	"log/slog"
)

// Fields mirrors logrus.Fields so vendored call sites need only an
// identifier rename (logrus.Fields{...} -> log.Fields{...}), not a rewrite.
type Fields = map[string]any

// Entry is a minimal logrus.Entry-alike backed by log/slog, carrying only
// the surface the vendored winhcs tree actually calls: WithField/WithFields/
// WithError plus the level methods used below. It does not reproduce
// logrus's context-aware span-annotation hook (internal/log/hook.go
// upstream) or its custom formatter (nopformatter.go) -- neither has any
// caller left once internal/oc/exporter.go (the logrus span exporter) is
// dropped, so both were cut rather than ported.
type Entry struct {
	logger *slog.Logger
	Data   Fields
}

func newEntry(logger *slog.Logger) *Entry {
	return &Entry{logger: logger, Data: Fields{}}
}

// L is the default, blank logging entry. WithField and co. all return a copy
// of the original entry, so this will not leak fields between calls.
var L = newEntry(slog.Default())

type entryContextKeyType struct{}

var entryContextKey entryContextKeyType

// G returns the log entry stored in the context by UpdateContext, or the
// default entry L if none was stored.
func G(ctx context.Context) *Entry {
	if e, ok := ctx.Value(entryContextKey).(*Entry); ok {
		return e
	}
	return L
}

// UpdateContext re-stores ctx's current entry (via G) back onto ctx itself,
// so it is reachable without walking up to a parent context.
func UpdateContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, entryContextKey, G(ctx))
}

func (e *Entry) clone() *Entry {
	data := make(Fields, len(e.Data))
	for k, v := range e.Data {
		data[k] = v
	}
	return &Entry{logger: e.logger, Data: data}
}

func (e *Entry) WithField(key string, value any) *Entry {
	c := e.clone()
	c.Data[key] = value
	return c
}

func (e *Entry) WithFields(fields Fields) *Entry {
	c := e.clone()
	for k, v := range fields {
		c.Data[k] = v
	}
	return c
}

func (e *Entry) WithError(err error) *Entry {
	return e.WithField("error", err)
}

func (e *Entry) args() []any {
	args := make([]any, 0, len(e.Data)*2)
	for k, v := range e.Data {
		args = append(args, k, v)
	}
	return args
}

// levelTrace sits below slog.LevelDebug, matching logrus's TraceLevel being
// strictly more verbose than DebugLevel -- so enabling Debug doesn't also
// turn on Trace, and vice versa.
const levelTrace = slog.LevelDebug - 4

// enabled reports whether level would actually be emitted. Every level
// method below checks this BEFORE building its message/fields, so a
// disabled level pays none of that cost -- matching logrus's
// IsLevelEnabled short-circuit, which this shim otherwise lacked.
func (e *Entry) enabled(level slog.Level) bool {
	return e.logger.Enabled(context.Background(), level)
}

// log emits msg at level with the entry's accumulated fields. Callers must
// have already checked enabled(level).
func (e *Entry) log(level slog.Level, msg string) {
	e.logger.Log(context.Background(), level, msg, e.args()...)
}

func (e *Entry) Trace(args ...any) {
	if e.enabled(levelTrace) {
		e.log(levelTrace, fmt.Sprint(args...))
	}
}

func (e *Entry) Debug(args ...any) {
	if e.enabled(slog.LevelDebug) {
		e.log(slog.LevelDebug, fmt.Sprint(args...))
	}
}

func (e *Entry) Debugf(format string, args ...any) {
	if e.enabled(slog.LevelDebug) {
		e.log(slog.LevelDebug, fmt.Sprintf(format, args...))
	}
}

func (e *Entry) Info(args ...any) {
	if e.enabled(slog.LevelInfo) {
		e.log(slog.LevelInfo, fmt.Sprint(args...))
	}
}

func (e *Entry) Warn(args ...any) {
	if e.enabled(slog.LevelWarn) {
		e.log(slog.LevelWarn, fmt.Sprint(args...))
	}
}

func (e *Entry) Warning(args ...any) { e.Warn(args...) }

func (e *Entry) Error(args ...any) {
	if e.enabled(slog.LevelError) {
		e.log(slog.LevelError, fmt.Sprint(args...))
	}
}

func (e *Entry) Errorf(format string, args ...any) {
	if e.enabled(slog.LevelError) {
		e.log(slog.LevelError, fmt.Sprintf(format, args...))
	}
}

// Package-level convenience wrappers over L, mirroring logrus's
// package-level API so call sites need only the import-identifier rename.
func WithError(err error) *Entry        { return L.WithError(err) }
func WithFields(fields Fields) *Entry   { return L.WithFields(fields) }
func Debug(args ...any)                 { L.Debug(args...) }
func Debugf(format string, args ...any) { L.Debugf(format, args...) }
