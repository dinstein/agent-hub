package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/internal/confops"
	"github.com/dinstein/agent-hub/internal/registry"
)

// The `scope` group edits the CLIENT layer of the four-layer chain
//: clients.json maps an agent application id to a profile
// binding plus its own narrowing rules. These are PERSISTENT bindings —
// the volatile per-connection layer is `session scope`.
//
// `scope` is an action group, so it keeps its name unchanged (no plural
// alias; canonical.md §3).

// ScopeBinding is one client's persisted scope binding.
type ScopeBinding struct {
	Client string `json:"client"`
	// Binding is the explicit profile reference: named / followActive.
	// It replaces toolport's `"profile": ""` magic value.
	Binding string `json:"binding"`
	Profile string `json:"profile,omitempty"`
	// Servers is the three-state narrowing set: null = no narrowing,
	// [] = block-all, [...] = intersect down to that set.
	Servers   []string                         `json:"servers"`
	Tools     map[string]registry.ToolSelector `json:"tools,omitempty"`
	Discovery string                           `json:"discovery,omitempty"`
	// Dangling is set when Binding names a profile that does not exist:
	// the resolver fail-closes it to an EMPTY scope, and doctor/`scope ls`
	// must say so out loud rather than show a silent empty set
	// (docs/architecture.md §7, improvement 5).
	Dangling bool `json:"dangling,omitempty"`
}

// ScopeList is the `scope ls` result.
type ScopeList struct {
	Bindings      []ScopeBinding `json:"bindings"`
	ActiveProfile string         `json:"active_profile"`
}

// Human renders the binding table.
func (l ScopeList) Human(w io.Writer) error {
	if len(l.Bindings) == 0 {
		_, err := fmt.Fprintln(w, "no client scope bindings (every client follows the active profile)")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "CLIENT\tBINDING\tSERVERS\tDISCOVERY\tTOOL RULES")
	for _, b := range l.Bindings {
		binding := b.Binding
		if b.Profile != "" {
			binding += ":" + b.Profile
		}
		if b.Dangling {
			binding += " (DANGLING -> empty scope)"
		}
		rules := "-"
		if len(b.Tools) > 0 {
			parts := make([]string, 0, len(b.Tools))
			for _, id := range sortedKeys(b.Tools) {
				parts = append(parts, id+": "+describeSelector(b.Tools[id]))
			}
			rules = strings.Join(parts, " | ")
		}
		disc := b.Discovery
		if disc == "" {
			disc = "-"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", b.Client, binding, describeServers(b.Servers), disc, rules)
	}
	return tw.Flush()
}

// ScopeSetResult is the `scope set` / `scope clear` result.
type ScopeSetResult struct {
	Action  string        `json:"action"`
	Client  string        `json:"client"`
	Binding *ScopeBinding `json:"binding,omitempty"`
}

// Human renders the outcome plus the resulting binding.
func (r ScopeSetResult) Human(w io.Writer) error {
	if _, err := fmt.Fprintf(w, "%s: client %s\n", r.Action, r.Client); err != nil {
		return err
	}
	if r.Binding == nil {
		return nil
	}
	return ScopeList{Bindings: []ScopeBinding{*r.Binding}}.Human(w)
}

func (a *App) newScopeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "scope",
		Short: "Bind AI clients to profiles and narrowing rules (persistent client layer)",
		Args:  cobra.ArbitraryArgs,
		RunE:  groupRunE,
	}
	cmd.AddCommand(a.newScopeLsCmd(), a.newScopeSetCmd(), a.newScopeClearCmd())
	return cmd
}

// scopeBindingOf projects one client entry, marking a dangling profile
// reference against the profile set it must resolve in.
func scopeBindingOf(id string, e registry.ClientEntry, profiles map[string]registry.Doc[registry.Profile]) ScopeBinding {
	b := e.Binding()
	out := ScopeBinding{
		Client:    id,
		Binding:   string(b.Kind),
		Profile:   b.Name,
		Servers:   e.Servers,
		Discovery: e.Discovery,
	}
	if len(e.Tools) > 0 {
		out.Tools = make(map[string]registry.ToolSelector, len(e.Tools))
		for sid, doc := range e.Tools {
			out.Tools[sid] = doc.V
		}
	}
	if b.Kind == registry.BindingNamed {
		if _, ok := profiles[b.Name]; !ok {
			out.Dangling = true
		}
	}
	return out
}

func (a *App) newScopeLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List every client scope binding",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, warnings, err := a.openStore()
			if err != nil {
				return err
			}
			active, err := a.activeProfile()
			if err != nil {
				return err
			}
			snap := store.Snapshot()
			list := ScopeList{ActiveProfile: active, Bindings: []ScopeBinding{}}
			for _, id := range sortedKeys(snap.Clients.V.Clients) {
				b := scopeBindingOf(id, snap.Clients.V.Clients[id].V, snap.Profiles.V.Profiles)
				if b.Dangling {
					warnings = append(warnings, fmt.Sprintf(
						"client %q references missing profile %q -> resolves to an EMPTY scope (fail-closed)", id, b.Profile))
				}
				list.Bindings = append(list.Bindings, b)
			}
			return a.printer().Emit(list, warnings...)
		},
	}
}

// scopeSetFlags groups `scope set`'s flags so the RunE body stays readable.
type scopeSetFlags struct {
	client       string
	profile      string
	followActive bool
	servers      []string
	tools        []string
	discovery    string
}

func (a *App) newScopeSetCmd() *cobra.Command {
	var f scopeSetFlags
	cmd := &cobra.Command{
		Use: "set --client <id> (--profile <p> | --follow-active | --servers a,b | " +
			"--tools s:t1,t2 | --discovery <mode>)",
		Short: "Set a client's profile binding and narrowing rules",
		Long: "Set one client's persistent scope binding.\n\n" +
			"Given flags are applied; omitted ones are left untouched, so the command\n" +
			"is usable both to create a binding and to amend one. Narrowing rules use\n" +
			"the same three-state semantics as profiles: --servers with no value means\n" +
			"block-all, and --tools s: (empty list) blocks every tool of s.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if f.client == "" {
				e := Usagef("--client is required")
				e.Hint = helpHint(cmd)
				return e
			}
			if f.profile != "" && f.followActive {
				e := Usagef("--profile and --follow-active are mutually exclusive")
				e.Hint = helpHint(cmd)
				return e
			}
			if !cmd.Flags().Changed("profile") && !f.followActive && !cmd.Flags().Changed("servers") &&
				!cmd.Flags().Changed("tools") && !cmd.Flags().Changed("discovery") {
				e := Usagef("nothing to set: pass at least one of --profile/--follow-active/--servers/--tools/--discovery")
				e.Hint = helpHint(cmd)
				return e
			}
			toolSpecs, err := parseToolSpecs(f.tools)
			if err != nil {
				return err
			}
			store, warnings, err := a.opsStore()
			if err != nil {
				return err
			}
			res, err := confops.SetClientBinding(
				cmd.Context(), store, f.client, scopeBindingPatch(cmd, f, toolSpecs), noPrecondition)
			warnings = append(warnings, res.Warnings...)
			if err != nil {
				return opsError(err)
			}
			binding := scopeBindingOf(f.client, res.Entry, store.Snapshot().Profiles.V.Profiles)
			return a.printer().Emit(ScopeSetResult{Action: "scope set", Client: f.client, Binding: &binding}, warnings...)
		},
	}
	cmd.Flags().StringVar(&f.client, "client", "", "client id to bind (scope routing key)")
	cmd.Flags().StringVar(&f.profile, "profile", "", "bind to this named profile")
	cmd.Flags().BoolVar(&f.followActive, "follow-active", false, "follow the globally active profile")
	cmd.Flags().StringSliceVar(&f.servers, "servers", nil, "narrow visibility to these servers (empty value = block all)")
	cmd.Flags().StringArrayVar(&f.tools, "tools", nil, "narrow one server's tools: <server>:<tool>[,<tool>] (repeatable)")
	cmd.Flags().StringVar(&f.discovery, "discovery", "", "discovery mode: lazy, grouped or full")
	return cmd
}

// scopeBindingPatch turns the typed flags into a confops patch. Only flags
// the user actually typed become fields (cobra's Changed), so an amend never
// silently resets a rule the operator did not mention — which is why the
// patch fields are pointers rather than plain values.
func scopeBindingPatch(cmd *cobra.Command, f scopeSetFlags, toolSpecs map[string][]string) confops.ClientBinding {
	var b confops.ClientBinding
	switch {
	case f.followActive:
		b.Profile = &confops.ProfileBindingSpec{Kind: registry.BindingFollowActive}
	case cmd.Flags().Changed("profile"):
		if f.profile == "" {
			// An explicit empty --profile means "stop naming a profile":
			// back to followActive rather than to the "" magic value.
			b.Profile = &confops.ProfileBindingSpec{Kind: registry.BindingFollowActive}
		} else {
			b.Profile = &confops.ProfileBindingSpec{Kind: registry.BindingNamed, Name: f.profile}
		}
	}
	if cmd.Flags().Changed("servers") {
		list := dedupSorted(f.servers)
		if list == nil {
			list = []string{}
		}
		b.Servers = &list
	}
	if cmd.Flags().Changed("tools") {
		b.Tools = map[string]confops.ToolSelection{}
		for id, tools := range toolSpecs {
			b.Tools[id] = toolSelectionFor(tools)
		}
	}
	if cmd.Flags().Changed("discovery") {
		discovery := f.discovery
		b.Discovery = &discovery
	}
	return b
}

func (a *App) newScopeClearCmd() *cobra.Command {
	var client string
	cmd := &cobra.Command{
		Use:   "clear --client <id>",
		Short: "Remove a client's scope binding (it falls back to the active profile)",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if client == "" {
				e := Usagef("--client is required")
				e.Hint = helpHint(cmd)
				return e
			}
			store, warnings, err := a.opsStore()
			if err != nil {
				return err
			}
			res, err := confops.ClearClientBinding(cmd.Context(), store, client, noPrecondition)
			warnings = append(warnings, res.Warnings...)
			if err != nil {
				return opsError(err)
			}
			return a.printer().Emit(ScopeSetResult{Action: "scope cleared", Client: client}, warnings...)
		},
	}
	cmd.Flags().StringVar(&client, "client", "", "client id whose binding is removed")
	return cmd
}

// validateDiscovery rejects an unknown discovery mode at the moment the
// operator can still fix it, instead of letting the resolver silently fall
// back to a default the operator did not ask for. The mode set itself is
// confops' to define, so `scope set`, `session scope` and `config set
// discovery` cannot accept three different vocabularies.
func validateDiscovery(mode string) error {
	return opsError(confops.ValidateDiscovery(mode))
}
