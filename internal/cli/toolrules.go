package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/registry"
)

// `server tool ls --rules` reads the allow lists themselves rather than their
// effect. It is the OLD home of that reading — the rule now belongs to the
// views that describe a server (`server ls`, `server inspect`, see
// serverrule.go) — and survives only so a script written against it keeps
// working for one release.
//
// Nothing new should be added here; this file goes away with the flag.

// ToolRuleRow is one server's global allow list.
type ToolRuleRow struct {
	Server  string `json:"server"`
	Enabled bool   `json:"enabled"`
	// The rule itself is inlined, so the JSON this flag has always emitted
	// is unchanged while the type behind it lives with the server.
	ServerToolRule
}

// ToolRuleList is the `server tool ls --rules` result. JSON shape: a plain array.
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
		_, _ = fmt.Fprintf(tw, "%s\t(%s)\t%s\n", name, r.rule(), r.rowTools())
	}
	return tw.Flush()
}

// rowTools is this table's TOOLS cell, unchanged from the day it shipped.
func (r ToolRuleRow) rowTools() string {
	switch r.Rule {
	case toolRuleAll:
		return "every tool the server offers"
	case toolRuleNone:
		return "no tools — registered but exposes nothing"
	}
	out := r.names()
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
			Server: id, Enabled: e.Enabled,
			ServerToolRule: serverToolRuleOf(e, cached[id]),
		})
	}
	return out
}
