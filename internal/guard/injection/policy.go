package injection

import "slices"

// Mode selects the enforcement strength (docs/flows.md: label is the
// default, block is opt-in).
type Mode int

// Enforcement modes. The zero value is ModeLabel on purpose: an unset policy
// must never silently block.
const (
	// ModeLabel annotates findings but always delivers the result.
	ModeLabel Mode = iota
	// ModeBlock rejects results that trigger at or above MinSeverity.
	ModeBlock
)

// Action is the enforcement decision ScanResult hands back to the pipeline.
type Action int

// Possible actions, in escalation order.
const (
	// ActionNone: no triggering findings (or exempt server) — pass through.
	ActionNone Action = iota
	// ActionLabel: deliver the result with the findings attached as labels.
	ActionLabel
	// ActionBlock: the pipeline must replace the result with a rejection.
	ActionBlock
)

// String implements fmt.Stringer.
func (a Action) String() string {
	switch a {
	case ActionNone:
		return "none"
	case ActionLabel:
		return "label"
	case ActionBlock:
		return "block"
	default:
		return "action(?)"
	}
}

// Policy configures enforcement for ScanResult. The zero value labels all
// findings and exempts nobody.
type Policy struct {
	Mode Mode
	// MinSeverity is the lowest severity that triggers the Mode action.
	// Zero means SeverityLow (everything triggers). Findings below the
	// threshold are still reported, they just do not escalate the Action.
	MinSeverity Severity
	// PerServerExempt lists server IDs whose results are not scanned at
	// all. Exemption is explicit operator configuration — the scanner
	// never infers it.
	PerServerExempt []string
}

func (p *Policy) exempts(serverID string) bool {
	return serverID != "" && slices.Contains(p.PerServerExempt, serverID)
}

// Result is the outcome of one ScanResult call.
type Result struct {
	Action   Action
	Findings []Finding
	// Exempted reports that the server was policy-exempt and nothing was
	// scanned (distinguishable from "scanned, clean" in audit).
	Exempted bool
}

// ScanResult is the single scan entry point for pipeline defend_and_shape.
// Success and error branches MUST both funnel through it (#421): on success
// the caller passes the text segments of the tool result, on error the error
// message — the API takes plain segments precisely so both branches share
// one shape.
//
// Failure direction: fail-open. Scanning itself never errors; an undetected
// payload passes unlabeled, and only ModeBlock with a triggering finding
// yields ActionBlock.
func (s *Scanner) ScanResult(pol Policy, serverID string, segments []string) Result {
	if pol.exempts(serverID) {
		return Result{Action: ActionNone, Exempted: true}
	}
	var all []Finding
	for i, seg := range segments {
		for _, f := range s.Scan(seg) {
			f.Segment = i
			all = append(all, f)
		}
	}
	if len(all) == 0 {
		return Result{Action: ActionNone}
	}
	minSev := pol.MinSeverity
	if minSev == 0 {
		minSev = SeverityLow
	}
	triggered := false
	for _, f := range all {
		if f.Severity >= minSev {
			triggered = true
			break
		}
	}
	switch {
	case !triggered:
		return Result{Action: ActionNone, Findings: all}
	case pol.Mode == ModeBlock:
		return Result{Action: ActionBlock, Findings: all}
	default:
		return Result{Action: ActionLabel, Findings: all}
	}
}
