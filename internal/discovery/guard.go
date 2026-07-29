package discovery

import (
	"fmt"
	"sync"
)

// SearchGuard breaks the search loop an injected or confused agent falls
// into: searching, getting the same top hit, and searching again instead of
// calling it (docs/flows.md failure branch).
//
// The state machine, exactly:
//
//   - ObserveSearch(top, score) with a NEW top name       → streak = 1
//   - ObserveSearch(top, score) with the SAME top name    → streak++
//   - ObserveSearch("", _) (no results)                   → streak = 0, top cleared
//   - streak >= EscalateAfter AND score >= ConfidenceThreshold → ESCALATE
//   - ObserveOther()  — ANY non-search action              → streak = 0
//   - Reset()         — scope change                       → streak = 0
//
// Two deliberate asymmetries:
//
//  1. A low-confidence top still ADVANCES the streak but never escalates.
//     Forcing an agent to call a tool the ranker barely believes in would
//     turn a weak guess into an instruction. If a later identical search
//     scores above the line, the accumulated streak escalates at once.
//
//  2. Escalation does not clear the streak. If the agent searches AGAIN
//     after being told to call the tool, it is told again — only doing
//     something else clears it. That is what "any non-search action resets"
//     means: the guard tracks a loop, and the loop ends when it ends.
//
// SearchGuard is per session (docs/architecture.md §7): a scope change invalidates the
// context in which "same top result" meant anything, hence Reset.
type SearchGuard struct {
	mu     sync.Mutex
	top    string
	streak int
}

// EscalateAfter is the number of consecutive searches with the same top
// result that triggers the imperative reply.
const EscalateAfter = 3

// maxStreak bounds the counter so a pathological session cannot overflow
// it; past the bound the behaviour is unchanged (already escalating).
const maxStreak = 1 << 20

// NewSearchGuard returns a guard in the clean state.
func NewSearchGuard() *SearchGuard { return &SearchGuard{} }

// Escalation is the guard's verdict on one search.
type Escalation struct {
	// Fire reports that this search must be answered with Message ALONE:
	// the result list is truncated to this single imperative line.
	Fire bool
	// Message is the frozen one-line instruction (empty unless Fire).
	Message string
	// Tool is the repeated top result.
	Tool string
	// Streak is the consecutive-identical-top count including this search.
	Streak int
	// Confident reports whether the top score cleared ConfidenceThreshold.
	Confident bool
}

// ObserveSearch records one search's outcome and reports the verdict.
// top is the rank-1 exposed name ("" when the search matched nothing) and
// score is its rank-1 score.
func (g *SearchGuard) ObserveSearch(top string, score int) Escalation {
	if g == nil {
		return Escalation{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	if top == "" {
		// Nothing found: there is no loop to break, and the next search is
		// a fresh attempt.
		g.top, g.streak = "", 0
		return Escalation{}
	}
	if top != g.top {
		g.top, g.streak = top, 1
	} else if g.streak < maxStreak {
		g.streak++
	}

	e := Escalation{
		Tool:      top,
		Streak:    g.streak,
		Confident: score >= ConfidenceThreshold,
	}
	if e.Streak >= EscalateAfter && e.Confident {
		e.Fire = true
		e.Message = escalationMessage(top)
	}
	return e
}

// ObserveOther records any NON-search action of the session — call_tool,
// fetch_result, status, a plain tools/list — and clears the streak. The
// gateway must call it on every such action: an agent that did something
// else is no longer looping.
func (g *SearchGuard) ObserveOther() { g.Reset() }

// Reset clears the guard. The gateway calls it on scope changes, where the
// accumulated history describes a tool surface that no longer exists.
func (g *SearchGuard) Reset() {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.top, g.streak = "", 0
	g.mu.Unlock()
}

// State reports the current top name and streak (diagnostics and tests).
func (g *SearchGuard) State() (top string, streak int) {
	if g == nil {
		return "", 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.top, g.streak
}

// escalationMessage is the frozen imperative line. One sentence, no
// alternatives offered, no restatement of the results: the whole point is
// that an agent stuck in a loop gets ONE instruction, not another menu.
func escalationMessage(tool string) string {
	return fmt.Sprintf("you already found %s; call it", tool)
}
