package eventlog

import (
	"context"
	"log/slog"

	"github.com/dinstein/agent-hub/internal/logx"
)

// The double write: every state change is BOTH a record here and a line in
// the process log, because the two readers need the same fact in two shapes —
// a value a UI can colour a timeline by, and prose a human reads in
// `agenthub logs`.
//
// It used to be two calls at every site, held together by a rule saying they
// must move together. Twenty-one kinds is twenty-one chances to remember it,
// and the failure is invisible from either side: a record with no prose reads
// as a silent gateway, a line with no record leaves a hole in the timeline.
// Emit makes it one call, which is the only version of that rule a compiler
// can hold up.

// Level is the severity of a kind.
//
// It is a property of WHAT HAPPENED, not of the sentence a call site chose to
// write about it, which is why it lives here beside the vocabulary rather
// than at each site. The convention is the repository's:
//
//   - Warn: degraded, still serving. Every failure kind is one of these —
//     the connection is down, the breaker is open, the token did not
//     refresh — because the hub carries on either way.
//   - Info: a state change that went as intended.
//
// Nothing is Error: that level is reserved for a protective capability
// failing (the ledger unavailable, rate-limit counters unusable), and no
// state change of a downstream is that. Nothing is Debug either — a kind
// worth a closed vocabulary is worth being visible at the default level.
//
// Kind alone, not the (scope, kind) pair the vocabulary is checked as: the
// two spellings two scopes share (`started`) mean the same thing at both, and
// severity does not depend on who the subject is. A kind that ever needs to
// differ by scope changes this signature.
func Level(kind Kind) slog.Level {
	switch kind {
	case KindConnectFailed, KindRespawnFailed, KindCircuitOpen, KindHealthDown,
		KindOAuthRefreshFailed, KindSecretsMissing, KindRegistryReloadFailed:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

// Emit appends r and writes the same fact to log, at the level r's kind
// decides. A nil stream still logs, and a nil logger still appends: neither
// half is the other's precondition.
//
// It stamps NO identity on the log record. The logger a call site holds is
// already bound to its server, instance, client and pid — slog's JSON handler
// does not deduplicate, so passing them again would emit each field twice on
// one line, and a reader taking the last (encoding/json included) reads the
// second value. What it does add is what varies per event: the transition,
// the number under the noun its kind gives it, the generation, the duration,
// and the detail — each under the same spelling the record uses, so one fact
// is not two names across the two streams.
//
// attrs are extra slog fields with no place in the closed record: the last
// words of a dead child process, the cause of a respawn. They belong in the
// prose half and nowhere else.
func (s *Stream) Emit(log *slog.Logger, r Record, msg string, attrs ...any) {
	s.Append(r)
	if log == nil {
		return
	}
	log.LogAttrs(context.Background(), Level(r.Kind), msg, recordAttrs(r, attrs)...)
}

// recordAttrs renders the varying half of a record as slog attrs.
func recordAttrs(r Record, extra []any) []slog.Attr {
	out := make([]slog.Attr, 0, 6+len(extra))
	if r.From != "" || r.To != "" {
		out = append(out, slog.String("from", r.From), slog.String("to", r.To))
	}
	if r.Count != 0 {
		// The noun CountNoun gives the kind, so the prose line and the record
		// cannot disagree about what the number counts — which they did while
		// the field was called `attempt` and three writers filled it with
		// three different quantities.
		noun := CountNoun(r.Kind)
		if noun == "" {
			noun = "count"
		}
		out = append(out, slog.Int(noun, r.Count))
	}
	if r.Rev != 0 {
		// logx.Rev, not the literal: `rev` is one of the mandatory field
		// names, and test/buildrules fails on a mandatory key spelled by hand
		// inside a slog call.
		out = append(out, logx.Rev(r.Rev))
	}
	if r.DurMs != 0 {
		out = append(out, slog.Int64("durMs", r.DurMs))
	}
	if r.Detail != "" {
		out = append(out, slog.String("detail", r.Detail))
	}
	for i := 0; i+1 < len(extra); i += 2 {
		key, _ := extra[i].(string)
		out = append(out, slog.Any(key, extra[i+1]))
	}
	return out
}
