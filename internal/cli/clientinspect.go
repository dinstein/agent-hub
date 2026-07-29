package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/internal/clients"
	"github.com/dinstein/agent-hub/internal/registry"
)

// `client inspect` is where "why does ls say that?" is answered. It OPENS
// the client's configuration files, which is the deliberate per-client
// action internal/clients reserves content reads for: one client, because
// the user named it, and a macOS privacy prompt here is explainable.

// ClientInspectServer is one entry in a client's server map.
type ClientInspectServer struct {
	Name      string `json:"name"`
	Transport string `json:"transport,omitempty"`
	Command   string `json:"command,omitempty"`
	URL       string `json:"url,omitempty"`
	Disabled  bool   `json:"disabled,omitempty"`
	// Owned marks agenthub's own gateway entry — decided by what the entry
	// says, never by its name.
	Owned bool `json:"owned"`
	// Stale marks an owned entry pointing at a binary that is not there:
	// connected on paper, broken in practice, and invisible to a check
	// that only asks whether the entry exists.
	Stale bool `json:"stale,omitempty"`
}

// ClientInspectFile is one configuration file of the inspected client.
type ClientInspectFile struct {
	Path      string `json:"path"`
	Placement string `json:"placement"`
	Exists    bool   `json:"exists"`
	// Parsed is false for a file that exists but was not read: a probe-only
	// shape, or one that could not be opened or understood (Error says
	// which). Servers is then empty because nobody looked, not because the
	// file is empty.
	Parsed    bool                  `json:"parsed"`
	Connected bool                  `json:"connected"`
	Servers   []ClientInspectServer `json:"servers,omitempty"`
	Error     string                `json:"error,omitempty"`
}

// ClientInspectView is the `client inspect` result.
type ClientInspectView struct {
	Client string `json:"client"`
	Name   string `json:"name,omitempty"`
	Shape  string `json:"shape,omitempty"`

	State      string   `json:"state"`
	Connected  bool     `json:"connected"`
	Placements []string `json:"placements,omitempty"`

	Binding  string `json:"binding"`
	Profile  string `json:"profile,omitempty"`
	Dangling bool   `json:"dangling,omitempty"`

	Files []ClientInspectFile `json:"files"`
	// Note explains a client agenthub cannot read or write, and Manual is
	// the fragment to paste into it by hand.
	Note   string `json:"note,omitempty"`
	Manual string `json:"manual,omitempty"`

	// ActiveProfile resolves the "(active: …)" a following client shows.
	ActiveProfile string `json:"active_profile,omitempty"`
}

// Human renders the per-file detail: what agenthub saw in each location and
// what it could not see.
func (v ClientInspectView) Human(w io.Writer) error {
	name := v.Client
	if v.Name != "" {
		name += " (" + v.Name + ")"
	}
	if _, err := fmt.Fprintf(w, "client:    %s\nconnected: %s\nprofile:   %s\n",
		name, connectedDetail(v), inspectProfileText(v)); err != nil {
		return err
	}
	for _, f := range v.Files {
		state := "absent"
		switch {
		case f.Error != "":
			state = f.Error
		case f.Exists && !f.Parsed:
			state = "present, not parsed"
		case f.Connected:
			state = "connected"
		case f.Exists:
			state = "no agenthub entry"
		}
		if _, err := fmt.Fprintf(w, "\n%s  [%s]  %s\n", f.Path, f.Placement, state); err != nil {
			return err
		}
		if len(f.Servers) == 0 {
			continue
		}
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		for _, s := range f.Servers {
			mark := " "
			if s.Owned {
				mark = "*"
			}
			target := s.Command
			if target == "" {
				target = s.URL
			}
			note := ""
			switch {
			case s.Stale:
				note = "  <- agenthub's entry, but that binary is not there"
			case s.Owned:
				note = "  <- agenthub's own entry"
			case s.Disabled:
				note = "  (disabled)"
			}
			_, _ = fmt.Fprintf(tw, "  %s %s\t%s\t%s%s\n", mark, s.Name, s.Transport, target, note)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	if v.Note != "" {
		if _, err := fmt.Fprintf(w, "\nnote: %s\n", v.Note); err != nil {
			return err
		}
	}
	if v.Manual != "" && !v.Connected {
		if _, err := fmt.Fprintf(w, "\nadd this by hand:\n%s", v.Manual); err != nil {
			return err
		}
	}
	return nil
}

// inspectProfileText spells out what this client may see. One line, so the
// fallback is written out rather than compressed into "(active: …)".
func inspectProfileText(v ClientInspectView) string {
	if v.Binding == string(registry.BindingNamed) {
		if v.Dangling {
			return v.Profile + "  MISSING -> resolves to an EMPTY scope"
		}
		return v.Profile
	}
	if v.ActiveProfile == "" {
		return "follows the active profile (none set: every enabled server is visible)"
	}
	return "follows the active profile: " + v.ActiveProfile
}

// connectedDetail spells out the connect state for a single client, where
// there is room to say what "?" is hiding.
func connectedDetail(v ClientInspectView) string {
	switch clients.ConnectState(v.State) {
	case clients.ConnectedYes:
		return "yes (" + strings.Join(v.Placements, ", ") + ")"
	case clients.ConnectedNo:
		return "no"
	case clients.ConnectedDenied:
		return "unknown — a configuration file could not be read"
	case clients.ConnectedUnreadable:
		return "unknown — a configuration file could not be parsed"
	default:
		return "unknown — agenthub does not read this client's format"
	}
}

func (a *App) newClientInspectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <client-id>",
		Short: "Open one client's config and report what is actually in it",
		Long: "This is the command that explains a 'client ls' row: every configuration\n" +
			"file this client may use, whether agenthub's entry is in it, and the other\n" +
			"MCP servers it already has.\n\n" +
			"Unlike 'client detect' it reads the files, so on macOS expect a privacy\n" +
			"prompt for the client you name.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			clientID := args[0]
			store, warnings, err := a.openStore()
			if err != nil {
				return err
			}
			active, err := a.activeProfile()
			if err != nil {
				return err
			}
			snap := store.Snapshot()
			insp, inspErr := clients.Default().Inspect(clientID, a.clientBaseDir())
			if inspErr != nil && len(insp.Files) == 0 {
				return classifyInspectError(inspErr, clientID)
			}

			b := clientBindingOf(clientID, snap.Clients.V.Clients[clientID].V, snap.Profiles.V.Profiles)
			if b.Dangling {
				warnings = append(warnings, fmt.Sprintf(
					"client %q is bound to missing profile %q -> resolves to an EMPTY scope (fail-closed)",
					clientID, b.Profile))
			}
			state, where := insp.ConnectState()
			view := ClientInspectView{
				Client: insp.Client, Name: insp.Name, Shape: string(insp.Shape),
				State: string(state), Connected: state == clients.ConnectedYes,
				Binding: b.Binding, Profile: b.Profile, Dangling: b.Dangling,
				Files: []ClientInspectFile{}, Note: insp.Note, Manual: insp.Manual,
				ActiveProfile: active,
			}
			for _, p := range where {
				view.Placements = append(view.Placements, string(p))
			}
			for _, f := range insp.Files {
				out := ClientInspectFile{
					Path: f.Path, Placement: string(f.Placement),
					Exists: f.Exists, Parsed: f.Parsed, Connected: f.Connected,
					Error: f.Error,
				}
				if f.Err != nil {
					warnings = append(warnings, clientID+": "+f.Error)
				}
				for _, s := range f.Servers {
					out.Servers = append(out.Servers, ClientInspectServer{
						Name: s.Name, Transport: s.Transport, Command: s.Command,
						URL: s.URL, Disabled: s.Disabled, Owned: s.Owned,
						// Only agenthub's own entry is judged: whether the
						// user's other servers resolve is their business.
						Stale: s.Owned && s.Command != "" && !binaryExists(s.Command),
					})
				}
				view.Files = append(view.Files, out)
			}
			return a.printer().Emit(view, warnings...)
		},
	}
}

// classifyInspectError maps an inspection that produced nothing at all onto
// the exit-code table. A failure that still yielded files is NOT routed
// here: those are reported per file and as warnings, because "one location
// is unreadable" must not suppress the ones that were fine.
func classifyInspectError(err error, clientID string) error {
	var unknown *clients.UnknownClientError
	if errors.As(err, &unknown) {
		e := NotFoundf(CodeClientUnsupported, "unknown client %q", clientID)
		e.Hint = "known clients: " + strings.Join(clients.IDs(), ", ")
		return e
	}
	return classifyClientsError(err)
}
