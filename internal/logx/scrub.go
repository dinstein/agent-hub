package logx

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
)

// Redacted replaces every scrubbed secret.
const Redacted = "[REDACTED]"

// Sensitive-value patterns, applied in order by ScrubString.
//
// Failure direction: fail-closed. False positives (over-redaction of a
// harmless value) are acceptable; a leaked credential is not. Do not narrow
// these patterns to make log output prettier.
var (
	// Keys whose name contains a sensitive word, in key=value / key: value
	// form (covers AGENTHUB_SECRET_*=..., api_key=..., Authorization: ...).
	// An optional "Bearer " prefix of the value is consumed so header lines
	// collapse to a single [REDACTED].
	sensitiveKVRe = regexp.MustCompile(
		`([A-Za-z0-9_.-]*(?i:secret|token|password|passwd|api[_-]?key|authorization|credential|access[_-]?key)[A-Za-z0-9_.-]*\s*[:=]\s*"?)((?i:bearer\s+)?[^\s"',;]+)`)

	// Bare bearer tokens in prose ("... sent Bearer abc123 ...").
	bearerRe = regexp.MustCompile(`(?i)\b(bearer\s+)[A-Za-z0-9._~+/=-]+`)

	// Well-known credential shapes, redacted wherever they appear.
	tokenShapeRe = regexp.MustCompile(
		`\b(?:sk-[A-Za-z0-9_-]{16,}` + // OpenAI/Anthropic-style API keys
			`|ghp_[A-Za-z0-9]{20,}|gho_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}` + // GitHub
			`|xox[baprs]-[A-Za-z0-9-]{10,}` + // Slack
			`|AKIA[0-9A-Z]{16}` + // AWS access key id
			`|ya29\.[A-Za-z0-9._-]{20,}` + // Google OAuth
			`|eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{6,}` + // JWT
			`)`)

	// Generic key=value where the value looks like a long random string
	// (>= 32 chars of base64-ish alphabet, no slashes to spare filesystem
	// paths). A post-match check requires both letters and digits.
	genericKVRe = regexp.MustCompile(`([A-Za-z0-9_.-]+=)([A-Za-z0-9+_-]{32,}={0,2})`)
)

// sensitiveKeyWords are matched against normalized attr keys (lowercase,
// "-"/"_" stripped). Any attr whose key contains one of these words has its
// whole value replaced, regardless of the value's kind or content.
var sensitiveKeyWords = []string{
	"secret", "token", "password", "passwd",
	"authorization", "apikey", "credential", "accesskey", "bearer",
}

// SensitiveKey reports whether an attr key must be fully masked.
func SensitiveKey(key string) bool {
	n := strings.ToLower(key)
	n = strings.ReplaceAll(n, "-", "")
	n = strings.ReplaceAll(n, "_", "")
	for _, w := range sensitiveKeyWords {
		if strings.Contains(n, w) {
			return true
		}
	}
	return false
}

// ScrubString redacts sensitive substrings from s. It is a pure function
// with no environment dependence — in particular AGENTHUB_DEBUG has no
// effect on it.
func ScrubString(s string) string {
	s = sensitiveKVRe.ReplaceAllString(s, "${1}"+Redacted)
	s = bearerRe.ReplaceAllString(s, "${1}"+Redacted)
	s = tokenShapeRe.ReplaceAllString(s, Redacted)
	s = genericKVRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := genericKVRe.FindStringSubmatch(m)
		if looksRandom(sub[2]) {
			return sub[1] + Redacted
		}
		return m
	})
	return s
}

// looksRandom requires at least one letter and one digit, filtering out
// long purely-alphabetic identifiers matched by genericKVRe.
func looksRandom(v string) bool {
	var hasLetter, hasDigit bool
	for _, r := range v {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			hasLetter = true
		}
	}
	return hasLetter && hasDigit
}

// ScrubHandler is a slog.Handler middleware that redacts secrets from the
// record message and from every attribute (including WithAttrs-bound ones
// and nested groups) before delegating to the wrapped handler.
//
// Invariant: scrubbing cannot be switched off. There is deliberately no
// config knob and no environment variable (AGENTHUB_DEBUG included) that
// bypasses it.
type ScrubHandler struct {
	next slog.Handler
}

// NewScrubHandler wraps next with secret scrubbing.
func NewScrubHandler(next slog.Handler) *ScrubHandler {
	return &ScrubHandler{next: next}
}

// Enabled implements slog.Handler.
func (h *ScrubHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

// Handle implements slog.Handler.
func (h *ScrubHandler) Handle(ctx context.Context, r slog.Record) error {
	nr := slog.NewRecord(r.Time, r.Level, ScrubString(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		nr.AddAttrs(scrubAttr(a))
		return true
	})
	return h.next.Handle(ctx, nr)
}

// WithAttrs implements slog.Handler; bound attrs are scrubbed eagerly so
// they are clean no matter which record they later attach to.
func (h *ScrubHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	scrubbed := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		scrubbed[i] = scrubAttr(a)
	}
	return &ScrubHandler{next: h.next.WithAttrs(scrubbed)}
}

// WithGroup implements slog.Handler.
func (h *ScrubHandler) WithGroup(name string) slog.Handler {
	return &ScrubHandler{next: h.next.WithGroup(name)}
}

// scrubAttr redacts a single attr. LogValuer values are resolved first so
// scrubbing sees the final value. Attrs with a sensitive key are masked
// entirely regardless of kind; otherwise string-ish values are pattern
// scrubbed and groups are walked recursively.
func scrubAttr(a slog.Attr) slog.Attr {
	a.Value = a.Value.Resolve()
	if SensitiveKey(a.Key) {
		a.Value = slog.StringValue(Redacted)
		return a
	}
	switch a.Value.Kind() {
	case slog.KindGroup:
		group := a.Value.Group()
		scrubbed := make([]slog.Attr, len(group))
		for i, ga := range group {
			scrubbed[i] = scrubAttr(ga)
		}
		a.Value = slog.GroupValue(scrubbed...)
	case slog.KindString:
		a.Value = slog.StringValue(ScrubString(a.Value.String()))
	case slog.KindAny:
		switch v := a.Value.Any().(type) {
		case string:
			a.Value = slog.StringValue(ScrubString(v))
		case error:
			// Errors frequently wrap request/header dumps; scrub their text.
			a.Value = slog.StringValue(ScrubString(v.Error()))
		}
	}
	return a
}
