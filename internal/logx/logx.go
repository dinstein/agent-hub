// Package logx initializes structured logging for agenthub on top of
// log/slog.
//
// It provides:
//   - Setup: a dual-handler logger — human-readable text to stderr and JSON
//     lines to a file, each independently switchable.
//   - The mandatory field convention (server / tool / client / session /
//     rev) as exported constants plus attr constructors.
//   - Secret scrubbing as a slog.Handler middleware (see scrub.go).
//
// Invariants (canonical.md §2, docs/modules/foundation.md):
//   - Zero business dependencies: standard library only (depguard-enforced).
//   - Scrubbing is unconditional. AGENTHUB_DEBUG=1 raises verbosity but
//     never bypasses redaction — secrets, tokens and credentials must not
//     reach any sink at any log level.
package logx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
)

// EnvDebug enables debug-level verbosity when set to "1". It is a frozen
// AGENTHUB_* identifier. It affects only the level filter — scrubbing is
// applied regardless.
const EnvDebug = "AGENTHUB_DEBUG"

// Config controls Setup. The zero value produces a logger that discards
// everything (both sinks disabled).
type Config struct {
	// TextEnabled turns on the human-readable text handler.
	TextEnabled bool
	// TextWriter receives text output. Defaults to os.Stderr.
	TextWriter io.Writer
	// JSONPath, when non-empty, turns on the JSON handler appending one
	// JSON object per line to the given file (created 0600 if missing).
	JSONPath string
	// Level is the minimum level for both sinks. Defaults to slog.LevelInfo.
	Level slog.Leveler
	// Debug forces debug-level verbosity, equivalent to AGENTHUB_DEBUG=1.
	// It does not — and must not — affect scrubbing.
	Debug bool
}

// Setup builds the logger described by cfg and returns it together with a
// close function releasing the JSON file (a no-op when JSON output is
// disabled). Every record passes through the scrubbing middleware before
// reaching any sink.
func Setup(cfg Config) (*slog.Logger, func() error, error) {
	level := slog.Leveler(slog.LevelInfo)
	if cfg.Level != nil {
		level = cfg.Level
	}
	if cfg.Debug || os.Getenv(EnvDebug) == "1" {
		level = slog.LevelDebug
	}
	opts := &slog.HandlerOptions{Level: level}

	var handlers []slog.Handler
	if cfg.TextEnabled {
		w := cfg.TextWriter
		if w == nil {
			w = os.Stderr
		}
		handlers = append(handlers, slog.NewTextHandler(w, opts))
	}

	closer := func() error { return nil }
	if cfg.JSONPath != "" {
		f, err := os.OpenFile(cfg.JSONPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, nil, fmt.Errorf("logx: open json log: %w", err)
		}
		handlers = append(handlers, slog.NewJSONHandler(f, opts))
		closer = f.Close
	}

	var h slog.Handler
	switch len(handlers) {
	case 0:
		h = slog.DiscardHandler
	case 1:
		h = handlers[0]
	default:
		h = multiHandler(handlers)
	}
	// Scrubbing wraps the outermost handler so that a single pass covers
	// every sink and every WithAttrs-bound attribute.
	return slog.New(NewScrubHandler(h)), closer, nil
}

// multiHandler fans one record out to several handlers. Handle errors are
// joined so one failing sink does not silence the others.
type multiHandler []slog.Handler

func (m multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (m multiHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, h := range m {
		if h.Enabled(ctx, r.Level) {
			if err := h.Handle(ctx, r.Clone()); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func (m multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := make(multiHandler, len(m))
	for i, h := range m {
		next[i] = h.WithAttrs(attrs)
	}
	return next
}

func (m multiHandler) WithGroup(name string) slog.Handler {
	next := make(multiHandler, len(m))
	for i, h := range m {
		next[i] = h.WithGroup(name)
	}
	return next
}
