package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/registry"
)

// `tool ls --rules` reads the allow lists themselves, rather than their
// effect.
//
// WHY IT IS NOT ANOTHER COLUMN ON THE TOOL TABLE. One state has no tools to
// hang a column on: a server set to --none contributes zero rows, so a
// per-tool listing cannot express "this server is configured to expose
// nothing" at all — the very state most worth finding. The rule table has one
// row per SERVER, so all three states have somewhere to be printed.
//
// It is also what makes `tool allow` safe to script against: the write
// REPLACES the list, so anything editing it has to read it first, and until
// this existed there was no command that could.

// ToolRuleRow is one server's global allow list.
type ToolRuleRow struct {
	Server  string `json:"server"`
	Enabled bool   `json:"enabled"`
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

// ToolRuleList is the `tool ls --rules` result. JSON shape: a plain array.
type ToolRuleList []ToolRuleRow

func (l ToolRuleList) MarshalJSON() ([]byte, error) {
	if l == nil {
		return []byte("[]"), nil
	}
	return json.Marshal([]ToolRuleRow(l))
}

// Human renders one row per server. The TOOLS cell spells out the two states
// that have no names to print, because an empty cell would read as the same
// thing in both — and they are opposites.
func (l ToolRuleList) Human(w io.Writer) error {
	if len(l) == 0 {
		_, err := fmt.Fprintln(w, "no servers configured ('agenthub server add' registers one)")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "SERVER\tRULE\tTOOLS")
	for _, r := range l {
		name := r.Server
		if !r.Enabled {
			// A rule on a disabled server describes nothing anyone can
			// reach; saying so here stops it being read as the reason.
			name += " (disabled)"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", name, r.rule(), r.tools())
	}
	return tw.Flush()
}

// rule is the RULE cell, carrying the count so "only" is never read without
// knowing how much it left out.
func (r ToolRuleRow) rule() string {
	if r.Rule != toolRuleOnly {
		return "(" + r.Rule + ")"
	}
	if r.Cached == 0 {
		// "2 of 0" is not a smaller total, it is the absence of one: no
		// gateway has connected to this server, so there is nothing to read
		// the count against and claiming a denominator would invent it.
		return fmt.Sprintf("only (%d, nothing cached)", len(r.Tools))
	}
	return fmt.Sprintf("only (%d of %d)", len(r.Tools), r.Cached)
}

func (r ToolRuleRow) tools() string {
	switch r.Rule {
	case toolRuleAll:
		return "every tool the server offers"
	case toolRuleNone:
		return "no tools — registered but exposes nothing"
	}
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
	out := strings.Join(parts, ", ")
	if len(r.Unknown) > 0 {
		out += "   (! no cached tool by that name)"
	}
	return out
}

// toolRulesOf builds the table from the registry and the tool cache. Every
// configured server gets a row, including the ones carrying no rule: "which
// of my servers is narrowed" cannot be answered by a list of the narrowed
// ones alone.
func toolRulesOf(
	servers map[string]registry.Doc[registry.ServerEntry],
	cached map[string][]mcp.ToolDef,
	serverArg string,
) ToolRuleList {
	out := make(ToolRuleList, 0, len(servers))
	for _, id := range sortedKeys(servers) {
		if serverArg != "" && id != serverArg {
			continue
		}
		e := servers[id].V
		out = append(out, ToolRuleRow{
			Server:  id,
			Enabled: e.Enabled,
			Rule:    ruleOf(e.Tools),
			Tools:   e.Tools,
			Unknown: unknownRuleNames(e.Tools, cached[id]),
			Cached:  len(cached[id]),
		})
	}
	return out
}
