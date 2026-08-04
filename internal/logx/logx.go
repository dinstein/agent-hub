// Package logx initializes structured logging for agenthub on top of
// log/slog.
//
// It provides:
//   - Setup: a dual-handler logger — human-readable text to stderr and JSON
//     lines to a writer the assembly supplies, each independently switchable.
//   - The mandatory field convention (server / tool / client / session /
//     rev / pid / inst) as exported constants plus attr constructors.
//   - Secret scrubbing as a slog.Handler middleware (see scrub.go).
//
// Invariants (canonical.md §2 rule 4, docs/modules/foundation.md):
//   - Zero business dependencies: standard library only (depguard-enforced).
//   - Scrubbing is unconditional. NOTHING that raises verbosity lowers
//     redaction: not AGENTHUB_DEBUG, not AGENTHUB_LOG_LEVEL, not
//     AGENTHUB_LOG_FILE_LEVEL, not Config.Debug or either Level field.
//     Secrets, tokens and credentials must not reach any sink at any level.
//     This holds structurally rather than by discipline — every one of those
//     inputs only decides a level, and NewScrubHandler wraps the outermost
//     handler after they are all resolved, so a new way in cannot get past
//     it by forgetting to. Naming them individually is how the list falls
//     behind the next one; the rule is the sentence, not the names.
package logx

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
)

// EnvDebug enables debug-level verbosity when set to "1". It is a frozen
// AGENTHUB_* identifier. It affects only the level filter — scrubbing is
// applied regardless.
const EnvDebug = "AGENTHUB_DEBUG"

// EnvLevel and EnvFileLevel name the level of each sink from outside the
// process: EnvLevel sets both, EnvFileLevel then overrides the JSON file
// alone. Together they spell the case EnvDebug cannot — file at debug while
// stderr stays quiet — which is what a stdio gateway needs, its stderr being
// read by the MCP client rather than by us.
//
// They accept what slog.Level.UnmarshalText accepts, case-insensitively:
// debug, info, warn, error, and offsets such as debug-2. Both are AGENTHUB_*
// and therefore stripped before a downstream server is spawned, so raising
// our own verbosity never reaches into someone else's process.
const (
	EnvLevel     = "AGENTHUB_LOG_LEVEL"
	EnvFileLevel = "AGENTHUB_LOG_FILE_LEVEL"
)

// envLevel reads one level-valued variable. Unset or empty returns nil, the
// "say nothing" value the resolution treats as absence.
//
// Failure direction: a value that does not parse is REPORTED AND IGNORED,
// never fatal. Logging is not the operation the process exists to perform,
// so a typo in a diagnostic knob must not stop a gateway from serving — but
// it must not pass silently either, because a level that did not apply and
// one that did look identical from inside, and the operator concludes the
// logs prove something they never recorded.
func envLevel(name string) (slog.Leveler, string) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return nil, ""
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(strings.ToUpper(raw))); err != nil {
		return nil, name + "=" + raw
	}
	return lvl, ""
}

// Config controls Setup. The zero value produces a logger that discards
// everything (both sinks disabled).
type Config struct {
	// TextEnabled turns on the human-readable text handler.
	TextEnabled bool
	// TextWriter receives text output. Defaults to os.Stderr.
	TextWriter io.Writer
	// JSON, when non-nil, turns on the JSON handler writing one JSON object
	// per line to it.
	//
	// This package does NOT open the file. It is locked to the standard
	// library, while the discipline a shared log file needs — a bounded line
	// written in one write(2), rotation by rename, retention — belongs to
	// internal/jsonl, which every other stream on disk already goes through.
	// The assembly opens jsonl.NewLineWriter and hands the sink over here,
	// and it also closes it: a sink this package neither opened nor sized is
	// not one whose lifetime it can decide.
	JSON io.Writer
	// Level is the minimum level for both sinks. Defaults to slog.LevelInfo.
	Level slog.Leveler
	// TextLevel and JSONLevel override Level for one sink each. nil means
	// "follow Level" — which is why they are overrides rather than the only
	// two fields: a caller that does not care about the distinction must not
	// have to state it twice, and the zero Config must keep meaning info.
	//
	// They exist because the two sinks have different audiences. The JSON
	// sink is a file this project owns, names and rotates; the text sink is
	// stderr, and a stdio gateway's stderr belongs to the MCP client that
	// spawned it, where our prose is noise in someone else's log. Raising
	// verbosity to diagnose something must therefore be able to reach the
	// file alone — otherwise the setting exists but nobody dares turn it on.
	TextLevel slog.Leveler
	JSONLevel slog.Leveler
	// Debug forces debug-level verbosity ON BOTH SINKS, equivalent to
	// AGENTHUB_DEBUG=1. It deliberately overrides the per-sink fields: it is
	// the blunt "show me everything" switch, and an assembly's own per-sink
	// choice silently holding it back would defeat the one thing it is
	// reached for. It does not — and must not — affect scrubbing.
	Debug bool
}

// levels resolves the minimum level of each sink, applying the documented
// precedence, widest last:
//
//	the info default
//	  < Config.Level < Config.TextLevel / Config.JSONLevel
//	  < AGENTHUB_LOG_LEVEL < AGENTHUB_LOG_FILE_LEVEL
//	  < Debug / AGENTHUB_DEBUG=1
//
// The environment sits above Config on purpose: Config is the assembly's
// standing choice, made when the binary was written, while the variables are
// the operator's, made in front of a problem. The one diagnosing has the
// later word.
//
// The returned strings name variables whose values did not parse, for the
// caller to report once the logger exists.
func (c Config) levels() (text, jsonSink slog.Leveler, bad []string) {
	if c.Debug || os.Getenv(EnvDebug) == "1" {
		return slog.LevelDebug, slog.LevelDebug, nil
	}
	base := slog.Leveler(slog.LevelInfo)
	if c.Level != nil {
		base = c.Level
	}
	text, jsonSink = base, base
	if c.TextLevel != nil {
		text = c.TextLevel
	}
	if c.JSONLevel != nil {
		jsonSink = c.JSONLevel
	}
	if lvl, complaint := envLevel(EnvLevel); lvl != nil {
		text, jsonSink = lvl, lvl
	} else if complaint != "" {
		bad = append(bad, complaint)
	}
	if lvl, complaint := envLevel(EnvFileLevel); lvl != nil {
		jsonSink = lvl
	} else if complaint != "" {
		bad = append(bad, complaint)
	}
	return text, jsonSink, bad
}

// Setup builds the logger described by cfg. Every record passes through the
// scrubbing middleware before reaching any sink.
//
// It returns no closer, because it holds nothing to close: both sinks are
// writers the caller supplied.
func Setup(cfg Config) *slog.Logger {
	textLevel, jsonLevel, badEnv := cfg.levels()

	// One HandlerOptions per sink, never one shared: a single value is what
	// tied the two levels together in the first place, and sharing it again
	// would silently undo the split the moment a handler is added.
	var handlers []slog.Handler
	if cfg.TextEnabled {
		w := cfg.TextWriter
		if w == nil {
			w = os.Stderr
		}
		handlers = append(handlers, slog.NewTextHandler(w, &slog.HandlerOptions{Level: textLevel}))
	}

	if cfg.JSON != nil {
		handlers = append(handlers, slog.NewJSONHandler(cfg.JSON, &slog.HandlerOptions{Level: jsonLevel}))
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
	logger := slog.New(NewScrubHandler(h))
	// An unparseable level is reported through the logger it failed to
	// configure, that being the only sink there is. Warn, so it clears the
	// info default rather than landing below it — a complaint filtered out
	// by the fallback would be no complaint at all. An assembly that pinned
	// its sinks to error keeps its silence; it did not ask a question.
	for _, bad := range badEnv {
		logger.Warn("ignoring an unreadable log level; using the default",
			"setting", bad, "expected", "debug, info, warn or error")
	}
	return logger
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
