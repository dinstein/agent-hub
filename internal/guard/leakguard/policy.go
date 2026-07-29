package leakguard

import (
	"fmt"
	"slices"
)

// Mode is the governance disposition (ruling #17). The ZERO VALUE IS
// ModeAudit on purpose: the async audit hook is the default-on half of the
// ruling, so a policy nobody configured audits — while inline rewriting, the
// half with semantic risk, can only be reached by naming it.
type Mode int

// Dispositions.
const (
	// ModeAudit scans off the call path and reports redacted records. The
	// delivered result is byte-identical to the downstream's.
	ModeAudit Mode = iota
	// ModeOff disables leak scanning entirely.
	ModeOff
	// ModeInline scans on the call path and replaces eligible spans with
	// Label(rule) before the result reaches the agent. Explicit opt-in.
	ModeInline
)

// String implements fmt.Stringer; the strings are the governance vocabulary
// ("leakguard: off | audit | inline").
func (m Mode) String() string {
	switch m {
	case ModeOff:
		return "off"
	case ModeAudit:
		return "audit"
	case ModeInline:
		return "inline"
	default:
		return fmt.Sprintf("mode(%d)", int(m))
	}
}

// MarshalText implements encoding.TextMarshaler.
func (m Mode) MarshalText() ([]byte, error) {
	switch m {
	case ModeOff, ModeAudit, ModeInline:
		return []byte(m.String()), nil
	default:
		return nil, fmt.Errorf("leakguard: invalid mode %d", int(m))
	}
}

// UnmarshalText implements encoding.TextUnmarshaler. It delegates to
// ParseMode, so an absent value decodes to the default (audit).
func (m *Mode) UnmarshalText(b []byte) error {
	parsed, err := ParseMode(string(b))
	if err != nil {
		return err
	}
	*m = parsed
	return nil
}

// ParseMode converts a governance string to a Mode. The empty string is the
// DEFAULT (audit), not "off": an unset governance key must leave the
// zero-latency audit hook on (#17).
//
// Failure direction: an unrecognised value is an ERROR, and the returned Mode
// is still ModeAudit — a typo in configuration must never silently disable
// the guard, so a caller that ignores the error keeps auditing.
func ParseMode(s string) (Mode, error) {
	switch s {
	case "", "audit":
		return ModeAudit, nil
	case "off":
		return ModeOff, nil
	case "inline":
		return ModeInline, nil
	default:
		return ModeAudit, fmt.Errorf("leakguard: unknown mode %q (want off|audit|inline)", s)
	}
}

// Policy configures one ScanResult call. The zero value is the shipped
// default: audit everything, redact nothing, exempt nobody.
type Policy struct {
	Mode Mode
	// MinSeverity is the lowest severity that is REPORTED. Zero means
	// SeverityLow (everything, including the entropy signal).
	MinSeverity Severity
	// MinRedactSeverity is the lowest severity inline mode rewrites. Zero
	// means SeverityMedium, which keeps the low-confidence entropy signal
	// out of the rewriting path even before its RedactNone strategy does
	// (belt and braces: two independent reasons, so neither one alone being
	// wrong can turn a heuristic into a mutation).
	MinRedactSeverity Severity
	// PerServerExempt lists server IDs whose results are not scanned at all.
	// Exemption is explicit operator configuration; it is never inferred.
	PerServerExempt []string
}

func (p *Policy) exempts(serverID string) bool {
	return serverID != "" && slices.Contains(p.PerServerExempt, serverID)
}

func (p *Policy) minSeverity() Severity {
	if p.MinSeverity == 0 {
		return SeverityLow
	}
	return p.MinSeverity
}

func (p *Policy) minRedactSeverity() Severity {
	if p.MinRedactSeverity == 0 {
		return SeverityMedium
	}
	return p.MinRedactSeverity
}

// Action is the disposition ScanResult hands back.
type Action int

// Possible actions, in escalation order.
const (
	// ActionNone: nothing to report — deliver the result untouched.
	ActionNone Action = iota
	// ActionAudit: findings exist and belong in the audit stream; the
	// result itself is unchanged.
	ActionAudit
	// ActionRedact: the caller MUST deliver Result.Segments instead of the
	// originals.
	ActionRedact
)

// String implements fmt.Stringer.
func (a Action) String() string {
	switch a {
	case ActionNone:
		return "none"
	case ActionAudit:
		return "audit"
	case ActionRedact:
		return "redact"
	default:
		return fmt.Sprintf("action(%d)", int(a))
	}
}

// Result is the outcome of one ScanResult call.
type Result struct {
	Action   Action
	Findings []Finding
	// Segments carries the rewritten segments, positionally matching the
	// input, and is non-nil ONLY when Action is ActionRedact.
	Segments []string
	// Redacted counts replaced spans across all segments.
	Redacted int
	// Exempted reports that the server was policy-exempt and nothing was
	// scanned — distinguishable in audit from "scanned, clean".
	Exempted bool
	// Truncated reports that more findings existed than the scanner's cap
	// reports. Redaction is NOT truncated: every segment is still rewritten
	// in full, only the report is bounded.
	Truncated bool
}

// ScanResult is the entry point the pipeline's defend_and_shape stage uses.
// Success and error branches both funnel through it — a hostile or careless
// server must not be able to smuggle a secret out inside an error message
// (the same reasoning as guard/injection #421).
//
// Failure direction: fail-open on detection (an unmatched secret is
// delivered), fail-closed on disposition (a finding eligible for redaction in
// inline mode is always replaced; when the caller cannot rewrite its payload
// it must withhold it rather than deliver it — see the pipeline's leak stage).
func (s *Scanner) ScanResult(pol Policy, serverID string, segments []string) Result {
	if pol.Mode == ModeOff {
		return Result{}
	}
	if pol.exempts(serverID) {
		return Result{Exempted: true}
	}
	minSev, minRed := pol.minSeverity(), pol.minRedactSeverity()
	var (
		all      []Finding
		out      []string
		redacted int
		dropped  bool
	)
	if pol.Mode == ModeInline {
		out = make([]string, len(segments))
	}
	for i, seg := range segments {
		fs := s.Scan(seg)
		kept := fs[:0]
		for _, f := range fs {
			if f.Severity >= minSev {
				f.Segment = i
				kept = append(kept, f)
			}
		}
		if pol.Mode == ModeInline {
			// Redaction reads the SCAN output (fs, pre-severity-filter is
			// irrelevant here since kept preserves order); rewriting uses the
			// same ascending, non-overlapping spans Redact requires.
			rewritten, n := Redact(seg, kept, minRed)
			out[i] = rewritten
			redacted += n
		}
		if s.maxFindings > 0 && len(all)+len(kept) > s.maxFindings {
			room := max(s.maxFindings-len(all), 0)
			kept = kept[:room]
			dropped = true
		}
		all = append(all, kept...)
	}
	res := Result{Findings: all, Redacted: redacted, Truncated: dropped}
	switch {
	case redacted > 0:
		res.Action = ActionRedact
		res.Segments = out
	case len(all) > 0:
		res.Action = ActionAudit
	}
	return res
}

// AuditRecord is what leaves this package for the audit stream: rule,
// severity, position, length. NO content, NO preview, NO excerpt — docs/modules/security.md
// 's red line is that audit never records the payload, and the async hook
// exists precisely so that a leak can be investigated without the audit trail
// becoming a second copy of the leak.
type AuditRecord struct {
	Rule     string   `json:"rule"`
	Severity Severity `json:"severity"`
	Segment  int      `json:"segment"`
	Start    int      `json:"start"`
	End      int      `json:"end"`
	Length   int      `json:"length"`
}

// Records projects findings onto audit records.
func Records(findings []Finding) []AuditRecord {
	if len(findings) == 0 {
		return nil
	}
	out := make([]AuditRecord, len(findings))
	for i, f := range findings {
		out[i] = AuditRecord{
			Rule:     f.Rule,
			Severity: f.Severity,
			Segment:  f.Segment,
			Start:    f.Start,
			End:      f.End,
			Length:   f.Length,
		}
	}
	return out
}
