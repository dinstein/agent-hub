package cli

import (
	"fmt"
	"strings"

	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/registry"
)

// ServerToolRule is one server's global tool allow list, as the SERVER's own
// views report it: a column in `server ls`, a line in `server inspect`.
//
// WHERE IT IS READ IS THE WHOLE POINT. The rule is a field on the server entry
// beside `enabled` (canonical.md §7), and for one release its only reader was
// `server tool ls --rules` — a boolean that swapped the row type, so one
// command answered with two JSON contracts, and the two commands that describe
// a server carried the field in neither. It is reported wherever the server is
// described now, which is also what makes the two altitudes read alike:
// `profile ls` has carried the same field, in a column, one layer down all
// along.
//
// The effect of the rule is a different question with a different shape — one
// row per TOOL — and it stays in `server tool ls`. Rule here, effect there.
type ServerToolRule struct {
	// Rule names the state in one word: all, only or none. It is derived
	// from Tools and duplicated on purpose — a consumer that switches on a
	// string cannot accidentally read null as empty.
	Rule string `json:"rule"`
	// Tools is the rule verbatim: null = no rule, [] = nothing, [...] =
	// exactly those. Never omitted; the null/empty distinction IS the
	// answer.
	Tools []string `json:"tools"`
	// Unknown are rule entries no cached tool matches. Absent when the
	// cache is cold, which is not the same as "all spelled right".
	Unknown []string `json:"unknown,omitempty"`
	// Cached is how many tools the last recorded catalog had, so "only 2"
	// can be read against a total rather than in a vacuum.
	Cached int `json:"cached"`
}

const (
	toolRuleAll  = "all"
	toolRuleOnly = "only"
	toolRuleNone = "none"
)

func ruleOf(tools []string) string {
	switch {
	case tools == nil:
		return toolRuleAll
	case len(tools) == 0:
		return toolRuleNone
	default:
		return toolRuleOnly
	}
}

// serverToolRuleOf reads one server's rule against its cached catalog. defs
// may be empty — a server no gateway has connected to yet — which costs the
// denominator and the spelling check, and neither is invented in its absence.
func serverToolRuleOf(e registry.ServerEntry, defs []mcp.ToolDef) ServerToolRule {
	return ServerToolRule{
		Rule:    ruleOf(e.Tools),
		Tools:   e.Tools,
		Unknown: unknownRuleNames(e.Tools, defs),
		Cached:  len(defs),
	}
}

// narrows reports whether this rule takes anything away. It is what decides
// the TOOLS column appears in `server ls` at all: a column reading "all" on
// every row for the rest of time teaches readers to stop seeing it, which is
// the same reason AUTH and TRACE come and go.
func (r ServerToolRule) narrows() bool { return r.Rule != toolRuleAll }

// cell is the `server ls` TOOLS cell: the state and, for `only`, how much of
// the catalog it left out. A trailing "!" says a named tool matches nothing —
// the marker is deliberately not a column of its own, because what has to be
// re-read to find the typo is the name, and `server inspect` prints it.
func (r ServerToolRule) cell() string {
	out := r.Rule
	if r.Rule == toolRuleOnly {
		// "?" rather than 0: no gateway has connected, so there is no total
		// to read the count against and printing one would invent it.
		total := "?"
		if r.Cached > 0 {
			total = fmt.Sprint(r.Cached)
		}
		out = fmt.Sprintf("only %d/%s", len(r.Tools), total)
	}
	if len(r.Unknown) > 0 {
		out += " !"
	}
	return out
}

// detail is the `server inspect` line: the state spelled out, and for `only`
// the names themselves, since that view is about this one server and the list
// is what the next `server tool allow` has to reproduce.
func (r ServerToolRule) detail() string {
	switch r.Rule {
	case toolRuleAll:
		return fmt.Sprintf("%s — every tool the server offers", r.rule())
	case toolRuleNone:
		return fmt.Sprintf("%s — registered but exposes nothing", r.rule())
	default:
		return fmt.Sprintf("%s — %s", r.rule(), r.names())
	}
}

// rule is the state with its count, so "only" is never read without knowing
// how much it left out.
func (r ServerToolRule) rule() string {
	if r.Rule != toolRuleOnly {
		return r.Rule
	}
	if r.Cached == 0 {
		// "2 of 0" is not a smaller total, it is the absence of one.
		return fmt.Sprintf("only (%d, nothing cached)", len(r.Tools))
	}
	return fmt.Sprintf("only (%d of %d)", len(r.Tools), r.Cached)
}

// names lists the rule's entries, marking the ones nothing matches.
func (r ServerToolRule) names() string {
	unknown := make(map[string]bool, len(r.Unknown))
	for _, u := range r.Unknown {
		unknown[u] = true
	}
	parts := make([]string, 0, len(r.Tools))
	for _, t := range r.Tools {
		if unknown[t] {
			// Marked inline rather than in a column of its own: the name is
			// what has to be re-read to spot the typo, so the marker belongs
			// against the name.
			t = "!" + t
		}
		parts = append(parts, t)
	}
	return strings.Join(parts, ", ")
}

// unknownRuleNames names the entries of an allow list that no cached tool
// matches — the spelling mistakes, and the only lasting symptom they have.
//
// An EMPTY catalog yields nothing, deliberately: every name would be
// reported, and a server no gateway has connected to yet is not a typo. That
// is the same silence unknownToolWarning keeps at write time, for the same
// reason, and the two must not disagree about it.
func unknownRuleNames(rule []string, defs []mcp.ToolDef) []string {
	if len(rule) == 0 || len(defs) == 0 {
		return nil
	}
	known := make(map[string]bool, len(defs))
	for _, d := range defs {
		known[d.Name] = true
	}
	var out []string
	for _, name := range rule {
		if !known[name] {
			out = append(out, name)
		}
	}
	return out
}
