// Package logging builds the gateway's logger. Every record passes through a
// Redactor, so a credential that reaches a log call is still not written out.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// Options configures the logger. Zero values mean "info" and "text".
type Options struct {
	Level  string // debug | info | warn | error
	Format string // text | json
	File   string // when set, append to this file (0600) instead of stderr
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// New returns a logger and a closer for the log file (if any).
func New(opts Options, red *Redactor) (*slog.Logger, io.Closer, error) {
	var (
		out    io.Writer = os.Stderr
		closer io.Closer = nopCloser{}
	)
	if opts.File != "" {
		f, err := os.OpenFile(opts.File, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, nil, fmt.Errorf("open log file: %w", err)
		}
		out, closer = f, f
	}

	lvl, err := ParseLevel(opts.Level)
	if err != nil {
		return nil, nil, err
	}
	hopts := &slog.HandlerOptions{Level: lvl}

	var h slog.Handler
	switch strings.ToLower(strings.TrimSpace(opts.Format)) {
	case "", "text":
		h = slog.NewTextHandler(out, hopts)
	case "json":
		h = slog.NewJSONHandler(out, hopts)
	default:
		return nil, nil, fmt.Errorf("unknown log format %q (want text or json)", opts.Format)
	}
	return slog.New(redactHandler{inner: h, red: red}), closer, nil
}

// ParseLevel maps a config string onto a slog level.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("unknown log level %q", s)
}

// Discard returns a logger that writes nowhere; used by tests and by CLI
// commands that report through stdout instead.
func Discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

type redactHandler struct {
	inner slog.Handler
	red   *Redactor
}

func (h redactHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h redactHandler) Handle(ctx context.Context, r slog.Record) error {
	nr := slog.NewRecord(r.Time, r.Level, h.red.Redact(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		nr.AddAttrs(h.attr(a))
		return true
	})
	return h.inner.Handle(ctx, nr)
}

func (h redactHandler) WithAttrs(as []slog.Attr) slog.Handler {
	out := make([]slog.Attr, len(as))
	for i, a := range as {
		out[i] = h.attr(a)
	}
	return redactHandler{inner: h.inner.WithAttrs(out), red: h.red}
}

func (h redactHandler) WithGroup(name string) slog.Handler {
	return redactHandler{inner: h.inner.WithGroup(name), red: h.red}
}

func (h redactHandler) attr(a slog.Attr) slog.Attr {
	a.Value = h.value(a.Value)
	return a
}

func (h redactHandler) value(v slog.Value) slog.Value {
	switch v.Kind() {
	case slog.KindString:
		return slog.StringValue(h.red.Redact(v.String()))
	case slog.KindGroup:
		as := v.Group()
		out := make([]slog.Attr, len(as))
		for i, a := range as {
			out[i] = h.attr(a)
		}
		return slog.GroupValue(out...)
	case slog.KindLogValuer:
		return h.value(v.Resolve())
	case slog.KindAny:
		switch t := v.Any().(type) {
		case error:
			return slog.StringValue(h.red.Redact(t.Error()))
		case fmt.Stringer:
			return slog.StringValue(h.red.Redact(t.String()))
		default:
			return slog.StringValue(h.red.Redact(fmt.Sprint(t)))
		}
	}
	return v
}
