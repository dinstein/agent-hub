package cli

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/internal/confops"
	"github.com/dinstein/agent-hub/internal/registry"
)

// `client bind` / `unbind` / `ls` own clients.json: which profile each agent
// application is on. That is the entire client layer — a client SELECTS a
// profile and never narrows on top of one, so "which profile is this client
// bound to" is a complete answer to "what can it see".
//
// These are PERSISTENT bindings; the volatile per-connection layer is
// `session scope`, which cannot outlive its session.

// ClientBindingView is one client's persisted profile binding.
type ClientBindingView struct {
	Client string `json:"client"`
	// Binding is the explicit profile reference: named / followActive.
	// It replaces toolport's `"profile": ""` magic value.
	Binding string `json:"binding"`
	Profile string `json:"profile,omitempty"`
	// Dangling is set when Binding names a profile that does not exist: the
	// resolver fail-closes it to an EMPTY scope, and this must be said out
	// loud rather than shown as a silent empty set
	// (docs/architecture.md §7, improvement 5).
	Dangling bool `json:"dangling,omitempty"`
}

// ClientBindingList is the `client ls` result.
type ClientBindingList struct {
	Bindings      []ClientBindingView `json:"bindings"`
	ActiveProfile string              `json:"active_profile"`
}

// Human renders the binding table.
func (l ClientBindingList) Human(w io.Writer) error {
	active := l.ActiveProfile
	if active == "" {
		active = "none (every enabled server is visible)"
	}
	if len(l.Bindings) == 0 {
		_, err := fmt.Fprintf(w,
			"no clients bound; every client follows the active profile: %s\n", active)
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "CLIENT\tPROFILE")
	for _, b := range l.Bindings {
		profile := b.Profile
		if b.Binding == string(registry.BindingFollowActive) {
			profile = "(active: " + active + ")"
		}
		if b.Dangling {
			profile += "  MISSING -> empty scope"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\n", b.Client, profile)
	}
	return tw.Flush()
}

// ClientBindResult is the `client bind` / `client unbind` result.
type ClientBindResult struct {
	Action  string             `json:"action"`
	Client  string             `json:"client"`
	Binding *ClientBindingView `json:"binding,omitempty"`
}

// Human renders the outcome plus the resulting binding.
func (r ClientBindResult) Human(w io.Writer) error {
	if _, err := fmt.Fprintf(w, "%s: client %s\n", r.Action, r.Client); err != nil {
		return err
	}
	if r.Binding == nil {
		return nil
	}
	return ClientBindingList{Bindings: []ClientBindingView{*r.Binding}}.Human(w)
}

// clientBindingOf projects one client entry, marking a dangling profile
// reference against the profile set it must resolve in.
func clientBindingOf(
	id string, e registry.ClientEntry, profiles map[string]registry.Doc[registry.Profile],
) ClientBindingView {
	b := e.Binding()
	out := ClientBindingView{Client: id, Binding: string(b.Kind), Profile: b.Name}
	if b.Kind == registry.BindingNamed {
		if _, ok := profiles[b.Name]; !ok {
			out.Dangling = true
		}
	}
	return out
}

func (a *App) newClientLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "Show which profile each client is on, and so what each one sees",
		Long: "A client on no profile of its own follows 'agenthub profile use'. One on a\n" +
			"profile that no longer exists sees nothing, and is flagged here as a warning.\n\n" +
			"This is what a client may see, not whether it is wired up at all — for that,\n" +
			"use 'agenthub client detect'.",
		Args: noArgs,
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
			list := ClientBindingList{ActiveProfile: active, Bindings: []ClientBindingView{}}
			for _, id := range sortedKeys(snap.Clients.V.Clients) {
				b := clientBindingOf(id, snap.Clients.V.Clients[id].V, snap.Profiles.V.Profiles)
				if b.Dangling {
					warnings = append(warnings, fmt.Sprintf(
						"client %q is bound to missing profile %q -> resolves to an EMPTY scope (fail-closed)",
						id, b.Profile))
				}
				list.Bindings = append(list.Bindings, b)
			}
			return a.printer().Emit(list, warnings...)
		},
	}
}

func (a *App) newClientBindCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bind <client-id> <profile>",
		Short: "Decide what one client sees by putting it on a profile",
		Long: "The profile then overrides the fallback from 'agenthub profile use', and the\n" +
			"client has no say in it. This applies immediately, even to a client already\n" +
			"running — only 'agenthub client connect' needs a restart.\n\n" +
			"Naming a profile that does not exist is allowed and warned about, but leaves\n" +
			"that client seeing nothing until it exists, so a typo can never widen access.",
		Args: exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, profile := args[0], args[1]
			store, warnings, err := a.opsStore()
			if err != nil {
				return err
			}
			binding := confops.ClientBinding{Profile: &confops.ProfileBindingSpec{
				Kind: registry.BindingNamed, Name: profile,
			}}
			res, err := confops.SetClientBinding(cmd.Context(), store, client, binding, noPrecondition)
			warnings = append(warnings, res.Warnings...)
			if err != nil {
				return opsError(err)
			}
			view := clientBindingOf(client, res.Entry, store.Snapshot().Profiles.V.Profiles)
			return a.printer().Emit(
				ClientBindResult{Action: "bound", Client: client, Binding: &view}, warnings...)
		},
	}
	return cmd
}

func (a *App) newClientUnbindCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unbind <client-id>",
		Short: "Take a client off its own profile, back onto the shared fallback",
		Long: "The client then follows 'agenthub profile use', or sees every enabled server\n" +
			"when that is unset — so this can WIDEN what it sees if it was on a narrow\n" +
			"profile. Check where it landed with 'agenthub client ls'.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := args[0]
			store, warnings, err := a.opsStore()
			if err != nil {
				return err
			}
			res, err := confops.ClearClientBinding(cmd.Context(), store, client, noPrecondition)
			warnings = append(warnings, res.Warnings...)
			if err != nil {
				return opsError(err)
			}
			return a.printer().Emit(
				ClientBindResult{Action: "unbound", Client: client}, warnings...)
		},
	}
}

// validateDiscovery rejects an unknown discovery mode at the moment the
// operator can still fix it, instead of letting the resolver silently fall
// back to a default the operator did not ask for. The mode set itself is
// confops' to define, so `profile discovery`, `session scope` and `config set
// discovery` cannot accept three different vocabularies.
func validateDiscovery(mode string) error {
	return opsError(confops.ValidateDiscovery(mode))
}
