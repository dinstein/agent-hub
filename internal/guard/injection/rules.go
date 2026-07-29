package injection

import (
	"fmt"
	"regexp"
)

// Severity ranks a finding. Higher is worse. The zero value is invalid so an
// uninitialized Rule fails validation instead of silently scanning at "low".
type Severity int

// Severity levels, low to high.
const (
	SeverityLow Severity = iota + 1
	SeverityMedium
	SeverityHigh
)

// String implements fmt.Stringer.
func (s Severity) String() string {
	switch s {
	case SeverityLow:
		return "low"
	case SeverityMedium:
		return "medium"
	case SeverityHigh:
		return "high"
	default:
		return fmt.Sprintf("severity(%d)", int(s))
	}
}

// MarshalText implements encoding.TextMarshaler (used by JSON golden files
// and audit records — the textual form is the stable one).
func (s Severity) MarshalText() ([]byte, error) {
	switch s {
	case SeverityLow, SeverityMedium, SeverityHigh:
		return []byte(s.String()), nil
	default:
		return nil, fmt.Errorf("injection: invalid severity %d", int(s))
	}
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (s *Severity) UnmarshalText(b []byte) error {
	switch string(b) {
	case "low":
		*s = SeverityLow
	case "medium":
		*s = SeverityMedium
	case "high":
		*s = SeverityHigh
	default:
		return fmt.Errorf("injection: unknown severity %q", b)
	}
	return nil
}

// Rule is one configurable detection rule. Exactly one of Phrase or Regex
// must be set. Both are matched against the normalized text form (lowercase,
// whitespace-collapsed, invisible characters stripped — see normalizeContent),
// so patterns should be written in lowercase with single spaces; `\s+` also
// works and matches the single collapsed space.
type Rule struct {
	// ID names the rule in findings and audit records. IDs of the built-in
	// table are stable (golden-tested); treat them as ABI once emitted.
	ID string
	// Phrase is a literal substring match on the normalized text.
	Phrase string
	// Regex is an RE2 pattern match on the normalized text.
	Regex    string
	Severity Severity
}

type compiledRule struct {
	Rule
	phrase string         // normalized Phrase, non-empty for phrase rules
	re     *regexp.Regexp // non-nil for regex rules
}

func compileRules(rules []Rule) ([]compiledRule, error) {
	out := make([]compiledRule, 0, len(rules))
	for i, r := range rules {
		if r.ID == "" {
			return nil, fmt.Errorf("injection: rule %d has no ID", i)
		}
		if r.Severity < SeverityLow || r.Severity > SeverityHigh {
			return nil, fmt.Errorf("injection: rule %q has invalid severity %d", r.ID, int(r.Severity))
		}
		switch {
		case r.Phrase != "" && r.Regex != "":
			return nil, fmt.Errorf("injection: rule %q sets both Phrase and Regex", r.ID)
		case r.Phrase != "":
			out = append(out, compiledRule{Rule: r, phrase: normalizeContent(r.Phrase).text})
		case r.Regex != "":
			re, err := regexp.Compile(r.Regex)
			if err != nil {
				return nil, fmt.Errorf("injection: rule %q: %w", r.ID, err)
			}
			out = append(out, compiledRule{Rule: r, re: re})
		default:
			return nil, fmt.Errorf("injection: rule %q sets neither Phrase nor Regex", r.ID)
		}
	}
	return out, nil
}

// DefaultRules returns the built-in detection table: instruction-override and
// system-prompt-injection classes inherited from the reference problem list
// . Callers get a fresh slice and may append or replace.
func DefaultRules() []Rule {
	return []Rule{
		{
			ID:       "override-instructions",
			Severity: SeverityHigh,
			Regex:    `(?:ignore|disregard|forget|override|bypass)\s+(?:all\s+|any\s+|the\s+|your\s+)*(?:previous|prior|above|earlier|preceding|original|system)\s+(?:instructions?|directions?|prompts?|rules?|messages?|context)`,
		},
		{
			ID:       "reveal-system-prompt",
			Severity: SeverityHigh,
			Regex:    `(?:reveal|show|print|repeat|output|display|leak|expose)\s+(?:your\s+|the\s+|its\s+)*system\s+prompt`,
		},
		{
			ID:       "disregard-safety",
			Severity: SeverityHigh,
			Regex:    `(?:disregard|ignore|without|remove)\s+(?:your\s+|the\s+|all\s+)*(?:guidelines|guardrails|safety|restrictions|policies)`,
		},
		{
			ID:       "chatml-marker",
			Severity: SeverityHigh,
			Regex:    `<\|im_(?:start|end)\|>`,
		},
		{
			ID:       "new-instructions",
			Severity: SeverityMedium,
			Regex:    `new\s+(?:instructions?|system\s+prompt)\s*:`,
		},
		{
			ID:       "persona-switch",
			Severity: SeverityMedium,
			Regex:    `you\s+are\s+now\s+(?:a|an|in|the|no\s+longer)\b`,
		},
		{
			ID:       "do-anything-now",
			Severity: SeverityMedium,
			Regex:    `do\s+anything\s+now|\bdan\s+mode\b`,
		},
		{
			ID:       "fake-system-tag",
			Severity: SeverityMedium,
			Regex:    `</?system>|\[/?(?:system|inst)\]`,
		},
		{
			ID:       "developer-mode",
			Severity: SeverityMedium,
			Phrase:   "developer mode",
		},
		{
			ID:       "system-prompt-mention",
			Severity: SeverityLow,
			Phrase:   "system prompt",
		},
		{
			ID:       "jailbreak",
			Severity: SeverityLow,
			Phrase:   "jailbreak",
		},
	}
}
