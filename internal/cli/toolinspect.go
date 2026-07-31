package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/internal/discovery"
	"github.com/dinstein/agent-hub/internal/gateway"
	"github.com/dinstein/agent-hub/internal/registry"
)

// `server tool inspect` answers the question the two listings can only answer by
// elimination: WHY is this one tool in, or out.
//
// `server tool ls` says a tool is blocked, `profile ls` shows the profiles, and
// joining them is left to the reader — per tool, in their head, in the same
// way `server inspect`'s visibility section was written to stop them doing
// for servers. This is that section at tool granularity, and it exists
// because the intersection is where the surprises are: a tool the global rule
// offers can still be missing from every client, and nothing in either
// listing says which layer took it.
//
// It is computed from the REGISTRY and the tool cache alone — no daemon, no
// client configuration file (that is `client inspect`'s deliberate act, and
// it can raise a macOS privacy prompt), so the answer stays available on the
// machine that is broken.

// ToolInspect is one tool, and every layer's verdict on it.
type ToolInspect struct {
	Name    string `json:"name"` // exposed name
	Server  string `json:"server"`
	RawName string `json:"rawName"`
	Enabled bool   `json:"serverEnabled"`

	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema,omitempty"`

	// Focus names the profile the report was narrowed to, empty for the
	// unfocused form. It is on the wire because Profiles carrying one entry
	// is otherwise indistinguishable from a machine with one profile.
	Focus string `json:"focus,omitempty"`

	// Global is the machine-wide verdict: allowed, blocked or pending.
	Global ToolVerdict `json:"global"`
	// Profiles is every profile's verdict, in name order. A profile that
	// does not include the server at all is reported too — "which profile
	// forgot this server" is not answerable from a list of the others.
	Profiles []ToolVerdict `json:"profiles,omitempty"`
	// Clients is what each bound client ends up with, and Default what a
	// client with no binding of its own follows.
	Clients []ToolClientVerdict `json:"clients,omitempty"`
	Default string              `json:"default"`
}

// ToolVerdict is one layer's answer, with the reason it gave.
type ToolVerdict struct {
	Layer string `json:"layer"` // "global" or a profile name
	// Allowed is the verdict. Reason names the rule that produced it, which
	// is the half that makes the verdict actionable.
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason"`
	Active  bool   `json:"active,omitempty"`
}

// ToolClientVerdict is one client's end result.
type ToolClientVerdict struct {
	Client  string `json:"client"`
	Profile string `json:"profile,omitempty"`
	Via     string `json:"via"`
	Sees    bool   `json:"sees"`
}

func (i ToolInspect) Human(w io.Writer) error {
	d := &detailWriter{w: w}
	if i.Focus != "" {
		// Said on the identity line, not further down: every section below is
		// narrowed, and a reader who missed the qualifier would take a report
		// about one profile for a report about the machine.
		d.line("%s (%s/%s), through profile %q", i.Name, i.Server, i.RawName, i.Focus)
	} else {
		d.line("%s (%s/%s)", i.Name, i.Server, i.RawName)
	}

	d.section("identity")
	d.field("server", "%s (enabled=%s)", i.Server, boolText(i.Enabled))
	d.field("raw name", "%s", i.RawName)
	if i.Description != "" {
		d.field("purpose", "%s", oneLine(i.Description, descriptionColumnBytes))
	}
	if len(i.Schema) > 0 {
		d.field("schema", "%s", string(i.Schema))
	}

	d.section("rules")
	d.field("global", "%s", verdictText(i.Global))
	for n, p := range i.Profiles {
		text := p.Layer
		if p.Active {
			text += " (active)"
		}
		d.at(n, "profiles", "%s: %s", text, verdictText(p))
	}
	d.field("default", "%s", i.Default)

	if len(i.Clients) > 0 {
		d.section("clients")
		var seen, hidden []string
		for _, c := range i.Clients {
			text := c.Client
			if c.Profile != "" {
				text += " (" + c.Profile + ")"
			}
			if c.Sees {
				seen = append(seen, text)
				continue
			}
			hidden = append(hidden, text)
		}
		if len(seen) > 0 {
			d.field("sees it", "%s", strings.Join(seen, ", "))
		}
		if len(hidden) > 0 {
			d.field("does not", "%s", strings.Join(hidden, ", "))
		}
	}
	return d.err
}

// verdictText renders one verdict. The reason always prints, including on a
// yes: "allowed" alone leaves the reader unable to tell a deliberate rule
// from the absence of one, which is precisely the pair they are checking.
func verdictText(v ToolVerdict) string {
	if v.Allowed {
		return "allowed — " + v.Reason
	}
	return "BLOCKED — " + v.Reason
}

func (a *App) newToolInspectCmd() *cobra.Command {
	var withSchema bool
	cmd := &cobra.Command{
		Use:   "inspect <tool> | <server> <tool>",
		Short: "Show one tool, and why each layer lets it through or does not",
		Long: "Resolves one tool and prints every verdict on it: the machine-wide allow\n" +
			"list, each profile, and what each client therefore gets.\n\n" +
			"The single-argument form takes the EXPOSED name a client sees\n" +
			"(github__get_issue). The two-argument form takes the server and the\n" +
			"server's own name, which is what to use when a name is ambiguous.\n\n" +
			"'agenthub profile tool inspect <profile> <tool>' asks the same question\n" +
			"from inside one profile, and answers it out of this same report.",
		Args: rangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.emitToolInspect(cmd, "", args, withSchema)
		},
	}
	cmd.Flags().BoolVar(&withSchema, "schema", false, "include the tool's raw input schema")
	return cmd
}

// newProfileToolInspectCmd is the same report entered from the profile side.
//
// It is an ALIAS, not a second implementation: inspect is inherently a
// cross-layer question — a tool the profile allows can still be missing
// because the machine-wide rule took it — so a per-profile computation would
// have to reproduce the layer above it and would eventually reproduce it
// wrongly. What the profile form changes is the FOCUS: the profiles section
// and the client list are narrowed to the one profile asked about, and the
// global verdict stays, because it can still be the answer.
func (a *App) newProfileToolInspectCmd() *cobra.Command {
	var withSchema bool
	cmd := &cobra.Command{
		Use:   "inspect <profile> <tool> | <profile> <server> <tool>",
		Short: "Show why this profile lets one tool through, or does not",
		Long: "The verdicts that decide one tool for this profile: the machine-wide allow\n" +
			"list above it, the profile's own two rules, and what the clients on this\n" +
			"profile therefore get.\n\n" +
			"The machine-wide layer is reported even though it is not the profile's:\n" +
			"it can be the layer that took the tool, and a report that left it out\n" +
			"would say a profile allows something no client can reach.\n\n" +
			"'agenthub server tool inspect <tool>' is the unfocused form — every\n" +
			"profile's verdict at once.",
		Args: rangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.emitToolInspect(cmd, args[0], args[1:], withSchema)
		},
	}
	cmd.Flags().BoolVar(&withSchema, "schema", false, "include the tool's raw input schema")
	return cmd
}

// emitToolInspect resolves and renders the report for both spellings. A
// profileName narrows the report to that profile after it is computed, never
// while: the layers that decide the tool are the same either way, and only
// what is PRINTED differs.
func (a *App) emitToolInspect(cmd *cobra.Command, profileName string, args []string, withSchema bool) error {
	store, warnings, err := a.openStore()
	if err != nil {
		return err
	}
	snap := store.Snapshot()
	cached, err := gateway.LoadToolCache(a.resolver, nil)
	if err != nil {
		return err
	}
	if profileName != "" {
		if _, ok := snap.Profiles.V.Profiles[profileName]; !ok {
			e := NotFoundf(CodeProfileNotFound, "no profile %q", profileName)
			e.Hint = "run 'agenthub profile ls' to see the profiles you have"
			return e
		}
	}
	serverArg := ""
	if len(args) == 2 {
		serverArg = args[0]
		if _, ok := snap.Servers.V.Servers[serverArg]; !ok {
			e := NotFoundf(CodeServerNotFound, "no server %q", serverArg)
			e.Hint = "run 'agenthub server ls' to see configured servers"
			return e
		}
	}
	// The catalog is built under the GLOBAL layer alone in both spellings, so
	// a tool this profile blocks is still resolvable: "why can this profile
	// not see it" is the question, and refusing the lookup answers it with
	// silence (the same reason resolveTool searches the blocked set).
	cat, err := offlineCatalogOf(snap, cached, serverArg, "")
	if err != nil {
		return err
	}
	tool, err := resolveTool(cat, args)
	if err != nil {
		return err
	}
	out := toolInspectOf(tool, snap, cat)
	if withSchema {
		out.Schema = tool.Def.InputSchema
	}
	if profileName != "" {
		out = out.focus(profileName, snap.Governance.V.ActiveProfile)
	}
	return a.printer().Emit(out, warnings...)
}

// focus narrows a report to one profile: its verdict, and the clients that
// end up on it. Everything above the profile is kept — it decides the same
// tool, and hiding it is how a report comes to claim a profile allows
// something no client can reach.
func (i ToolInspect) focus(name, active string) ToolInspect {
	i.Focus = name
	for _, p := range i.Profiles {
		if p.Layer == name {
			i.Profiles = []ToolVerdict{p}
			break
		}
	}
	var clients []ToolClientVerdict
	for _, c := range i.Clients {
		// A client with no binding of its own follows the ACTIVE profile, so
		// it belongs to this report exactly when this profile is the active
		// one — which is also when Default below is about it.
		if c.Profile == name || (c.Profile == "" && name == active) {
			clients = append(clients, c)
		}
	}
	i.Clients = clients
	// The default line answers "what does a client with no binding get", and
	// unfocused that is always about this report. Focused on a profile that
	// is NOT the active one it would answer about a different profile
	// entirely, under a heading the reader is holding as this one's.
	if name != active {
		if !i.Global.Allowed {
			i.Default = "nothing sees it: the machine-wide rule blocks it above every profile"
		} else if active == "" {
			i.Default = fmt.Sprintf(
				"no active profile — only a client bound to %q reads this verdict", name)
		} else {
			i.Default = fmt.Sprintf(
				"an unbound client follows %q, not this profile", active)
		}
	}
	return i
}

// resolveTool finds the named tool among everything cached, blocked included:
// "why can nobody see this" is the question, and refusing to look up a tool
// because it is currently invisible answers it with silence.
//
// The exposed name is matched WHOLE, never split on the join separator: a
// server id or a tool name may contain one, and splitting is how a lookup
// starts inventing provenance (docs/architecture.md, RouteOf is the only
// legitimate source).
func resolveTool(cat offlineCatalog, args []string) (discovery.Tool, error) {
	all := append(append([]discovery.Tool{}, cat.visible...), cat.blocked...)
	if len(args) == 2 {
		for _, t := range all {
			if t.ServerID == args[0] && t.RawTool == args[1] {
				return t, nil
			}
		}
		e := NotFoundf(CodeToolNotFound, "server %q has no cached tool %q", args[0], args[1])
		e.Hint = "run 'agenthub server tool ls " + args[0] + " --all' to see what it has"
		return discovery.Tool{}, e
	}
	for _, t := range all {
		if t.Exposed == args[0] {
			return t, nil
		}
	}
	e := NotFoundf(CodeToolNotFound, "no tool exposed as %q", args[0])
	e.Hint = "run 'agenthub server tool ls --all' to see the exposed names, " +
		"or name the server and its own tool name as two arguments"
	return discovery.Tool{}, e
}

// toolInspectOf joins the layers for one tool. It walks the same three-state
// selector rules the scope chain applies (nil = no narrowing, [] = nothing,
// [...] = that set) and fails CLOSED in the same direction: a binding to a
// profile that does not exist sees nothing, rather than falling back to a
// wider answer.
func toolInspectOf(t discovery.Tool, snap *registry.Snapshot, cat offlineCatalog) ToolInspect {
	entry := snap.Servers.V.Servers[t.ServerID].V
	active := snap.Governance.V.ActiveProfile
	out := ToolInspect{
		Name: t.Exposed, Server: t.ServerID, RawName: t.RawTool,
		Enabled: entry.Enabled, Description: t.Def.Description,
		Global: globalToolVerdict(entry, t.RawTool),
	}

	profiles := snap.Profiles.V.Profiles
	for _, name := range sortedKeys(profiles) {
		p := profiles[name].V
		v := ToolVerdict{Layer: name, Active: name == active}
		switch {
		case !profileIncludesServer(p, t.ServerID):
			v.Reason = "the profile does not include " + t.ServerID
		default:
			sel, ok := p.Tools[t.ServerID]
			v.Allowed, v.Reason = selectorVerdict(ok, sel.V.Allow, t.RawTool)
		}
		out.Profiles = append(out.Profiles, v)
	}

	for _, name := range sortedKeys(snap.Clients.V.Clients) {
		b := clientBindingOf(name, snap.Clients.V.Clients[name].V, profiles)
		out.Clients = append(out.Clients, ToolClientVerdict{
			Client: name, Profile: b.Profile, Via: b.Binding,
			Sees: out.Global.Allowed && entry.Enabled &&
				profileAllowsTool(profiles, b, active, t.ServerID, t.RawTool),
		})
	}
	out.Default = defaultToolText(out, active)
	return out
}

// globalToolVerdict reads the machine-wide rule. A name in the rule that no
// catalog has is reported as such rather than as "allowed": the rule does
// name it, and it still lets nothing through.
func globalToolVerdict(e registry.ServerEntry, raw string) ToolVerdict {
	v := ToolVerdict{Layer: "global"}
	v.Allowed, v.Reason = selectorVerdict(true, e.Tools, raw)
	if v.Reason == "" {
		v.Reason = "no allow list on the server"
	}
	return v
}

// selectorVerdict is the three-state read, shared by both layers so neither
// can answer differently. present=false means the layer has no entry at all,
// which is "no narrowing" and must never be read as the empty list.
func selectorVerdict(present bool, allow []string, raw string) (bool, string) {
	switch {
	case !present || allow == nil:
		return true, "no tool narrowing for this server"
	case len(allow) == 0:
		return false, "the rule allows no tools at all"
	case slices.Contains(allow, raw):
		return true, fmt.Sprintf("named by the allow list (%d tool(s))", len(allow))
	default:
		return false, "the allow list names " + strings.Join(allow, ", ")
	}
}

// profileAllowsTool resolves one binding down to a yes/no for this tool,
// fail-closed on a dangling reference (docs/architecture.md §7).
func profileAllowsTool(
	profiles map[string]registry.Doc[registry.Profile],
	b ClientBindingView, active, server, raw string,
) bool {
	name := b.Profile
	if b.Binding != string(registry.BindingNamed) {
		if active == "" {
			return true // no active profile narrows nothing
		}
		name = active
	}
	p, ok := profiles[name]
	if !ok {
		return false
	}
	if !profileIncludesServer(p.V, server) {
		return false
	}
	sel, has := p.V.Tools[server]
	allowed, _ := selectorVerdict(has, sel.V.Allow, raw)
	return allowed
}

// defaultToolText states what a client with no binding gets. It prints on
// every report rather than only when it changes the answer: which clients are
// bound is exactly what the reader does not know, and an unstated default
// reads as "does not apply here".
func defaultToolText(i ToolInspect, active string) string {
	if !i.Global.Allowed {
		return "nothing sees it: the machine-wide rule blocks it above every profile"
	}
	if active == "" {
		return "no active profile — an unbound client sees it"
	}
	for _, p := range i.Profiles {
		if p.Active {
			if p.Allowed {
				return fmt.Sprintf("an unbound client follows %q, which allows it", active)
			}
			return fmt.Sprintf("an unbound client follows %q, which BLOCKS it", active)
		}
	}
	return fmt.Sprintf("the active profile %q does not exist — unbound clients see nothing at all", active)
}
