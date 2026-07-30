package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/internal/confops"
	"github.com/dinstein/agent-hub/internal/discovery"
	"github.com/dinstein/agent-hub/internal/registry"
)

// The `profile` group is the front end of the profile operations in
// internal/confops: named capability tiers made of an enabled-server set
// plus per-server three-state tool selectors (docs/modules/controlplane.md, docs/architecture.md §7).
//
// The rules — what a rename does to the clients that reference the profile,
// what a removal deliberately does NOT do to them — live in confops, so the
// CLI and the control plane cannot answer them differently. What is left
// here is flag parsing and rendering.

// ProfileRow is one profile as both output modes render it.
type ProfileRow struct {
	Name string `json:"name"`
	// Servers is the three-state server set: null = no narrowing (every
	// registered server), [] = none, [...] = that set.
	Servers []string `json:"servers"`
	// Tools maps serverID -> selector for the servers this profile narrows.
	Tools map[string]registry.ToolSelector `json:"tools,omitempty"`
	// Discovery is how this profile's tools are surfaced; "" inherits the
	// global default.
	Discovery string `json:"discovery,omitempty"`
	// Active marks the globally active profile (`profile use`).
	Active bool `json:"active"`
}

// ProfileList is the `profile ls` result.
type ProfileList struct {
	Profiles []ProfileRow `json:"profiles"`
	// ActiveProfile is "" when none is set (equivalent to `profile use -`).
	ActiveProfile string `json:"active_profile"`
}

// Human renders the profile table.
func (l ProfileList) Human(w io.Writer) error {
	if len(l.Profiles) == 0 {
		_, err := fmt.Fprintln(w, "no profiles configured")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tACTIVE\tSERVERS\tDISCOVERY\tTOOL RULES")
	for _, p := range l.Profiles {
		rules := "-"
		if len(p.Tools) > 0 {
			parts := make([]string, 0, len(p.Tools))
			for _, id := range sortedKeys(p.Tools) {
				parts = append(parts, id+": "+describeSelector(p.Tools[id]))
			}
			rules = strings.Join(parts, " | ")
		}
		active := ""
		if p.Active {
			active = "*"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			p.Name, active, describeServers(p.Servers), dash(p.Discovery), rules)
	}
	return tw.Flush()
}

// ProfileChange is the result of every mutating profile subcommand: the
// profile as it now stands, plus what happened to it.
type ProfileChange struct {
	Action  string      `json:"action"`
	Name    string      `json:"name"`
	OldName string      `json:"old_name,omitempty"`
	Profile *ProfileRow `json:"profile,omitempty"`
	// Repointed lists client entries whose profile reference was rewritten
	// by a rename (leaving them dangling would fail-close them to an empty
	// scope, docs/architecture.md §7).
	Repointed []string `json:"repointed,omitempty"`
}

// Human renders the change.
func (c ProfileChange) Human(w io.Writer) error {
	switch c.Action {
	case "renamed":
		if _, err := fmt.Fprintf(w, "renamed: %s -> %s\n", c.OldName, c.Name); err != nil {
			return err
		}
	case "cleared":
		_, err := fmt.Fprintln(w, "active profile cleared (all registered servers visible)")
		return err
	default:
		if _, err := fmt.Fprintf(w, "%s: %s\n", c.Action, c.Name); err != nil {
			return err
		}
	}
	for _, id := range c.Repointed {
		if _, err := fmt.Fprintf(w, "repointed client %s\n", id); err != nil {
			return err
		}
	}
	if c.Profile != nil {
		_, err := fmt.Fprintf(w, "servers: %s\n", describeServers(c.Profile.Servers))
		return err
	}
	return nil
}

func (a *App) newProfileCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "profile",
		Aliases: []string{"profiles"}, // singular canonical, plural alias (canonical.md §3)
		Short:   "Build named sets of servers and tools to hand to a client",
		Long: "Building one up:\n" +
			"  agenthub profile create readonly\n" +
			"  agenthub profile server add readonly github\n" +
			"  agenthub client bind claude-code readonly\n\n" +
			"A profile only ever takes things away: it cannot show a server you disabled,\n" +
			"nor invent a tool. Clients you have not bound follow 'agenthub profile use',\n" +
			"or see every enabled server when that is unset.",
		Args: cobra.ArbitraryArgs,
		RunE: groupRunE,
	}
	cmd.AddCommand(
		a.newProfileLsCmd(),
		a.newProfileCreateCmd(),
		a.newProfileRmCmd(),
		a.newProfileRenameCmd(),
		a.newProfileUseCmd(),
		a.newProfileServerCmd(),
		a.newProfileToolsCmd(),
		a.newProfileDiscoveryCmd(),
	)
	return cmd
}

// profileRow projects one registry profile into its rendered form.
func profileRow(name string, p registry.Profile, active string) ProfileRow {
	row := ProfileRow{
		Name: name, Servers: p.Servers, Discovery: p.Discovery,
		Active: name == active && active != "",
	}
	if len(p.Tools) > 0 {
		row.Tools = make(map[string]registry.ToolSelector, len(p.Tools))
		for id, doc := range p.Tools {
			row.Tools[id] = doc.V
		}
	}
	return row
}

func (a *App) newProfileLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List your profiles and what each one includes",
		Long: "In the servers column, \"(all registered)\" means the profile takes nothing away\n" +
			"and \"(none)\" means a client bound to it sees nothing. For which client is on\n" +
			"which profile, use 'agenthub client ls'.",
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
			list := ProfileList{ActiveProfile: active, Profiles: []ProfileRow{}}
			for _, name := range sortedKeys(snap.Profiles.V.Profiles) {
				list.Profiles = append(list.Profiles, profileRow(name, snap.Profiles.V.Profiles[name].V, active))
			}
			if active != "" {
				if _, ok := snap.Profiles.V.Profiles[active]; !ok {
					warnings = append(warnings, fmt.Sprintf(
						"active profile %q does not exist; every followActive binding fail-closes to an empty scope", active))
				}
			}
			return a.printer().Emit(list, warnings...)
		},
	}
}

func (a *App) newProfileCreateCmd() *cobra.Command {
	var servers []string
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a new profile, empty unless you name its servers",
		Long: "A new profile takes nothing away until you say what it includes, so a client\n" +
			"bound to a fresh one still sees everything. Fill it in with 'agenthub profile\n" +
			"server add', or up front with --servers.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, warnings, err := a.opsStore()
			if err != nil {
				return err
			}
			res, err := confops.CreateProfile(cmd.Context(), store, args[0], servers, noPrecondition)
			warnings = append(warnings, res.Warnings...)
			if err != nil {
				return opsError(err)
			}
			row := profileRow(res.Name, res.Profile, "")
			return a.printer().Emit(ProfileChange{Action: "created", Name: res.Name, Profile: &row}, warnings...)
		},
	}
	cmd.Flags().StringSliceVar(&servers, "servers", nil,
		"the servers to include from the start; leave it out to include everything, or pass an empty value to include nothing")
	return cmd
}

func (a *App) newProfileRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "rm <name>",
		Aliases: []string{"remove"},
		Short:   "Delete a profile, leaving any client bound to it with nothing",
		Long: "Clients bound to it are not moved, and a client pointing at a profile that no\n" +
			"longer exists sees nothing at all — deliberately, so a deletion never widens\n" +
			"access. They are named in the output; move them with 'agenthub client bind'\n" +
			"or 'agenthub client unbind'.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			store, warnings, err := a.opsStore()
			if err != nil {
				return err
			}
			res, err := confops.RemoveProfile(cmd.Context(), store, name, noPrecondition)
			warnings = append(warnings, res.Warnings...)
			if err != nil {
				return opsError(err)
			}
			return a.printer().Emit(ProfileChange{Action: "removed", Name: name}, warnings...)
		},
	}
}

func (a *App) newProfileRenameCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rename <old> <new>",
		Short: "Rename a profile, moving every client bound to it along with it",
		Args:  exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, warnings, err := a.opsStore()
			if err != nil {
				return err
			}
			res, err := confops.RenameProfile(cmd.Context(), store, args[0], args[1], noPrecondition)
			warnings = append(warnings, res.Warnings...)
			if err != nil {
				return opsError(err)
			}
			return a.printer().Emit(ProfileChange{
				Action: "renamed", OldName: res.OldName, Name: res.Name, Repointed: res.Repointed,
			}, warnings...)
		},
	}
}

func (a *App) newProfileUseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   `use <name|->`,
		Short: "Choose the profile clients get when they have no profile of their own",
		Long: "A client bound with 'agenthub client bind' keeps its own profile and ignores\n" +
			"this. Passing \"-\" clears the fallback, so unbound clients go back to seeing\n" +
			"every enabled server.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			store, warnings, err := a.opsStore()
			if err != nil {
				return err
			}
			if name == "-" {
				// Clearing still writes the registry — the marker lives there
				// now — so it needs the store like any other edit.
				if _, cerr := confops.SetActiveProfile(cmd.Context(), store, "", noPrecondition); cerr != nil {
					return opsError(cerr)
				}
				return a.printer().Emit(ProfileChange{Action: "cleared"}, warnings...)
			}
			res, err := confops.SetActiveProfile(cmd.Context(), store, name, noPrecondition)
			warnings = append(warnings, res.Warnings...)
			if err != nil {
				return opsError(err)
			}
			row := profileRow(res.Name, res.Profile, res.Name)
			return a.printer().Emit(ProfileChange{Action: "active", Name: res.Name, Profile: &row}, warnings...)
		},
	}
}

func (a *App) newProfileServerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "server",
		Aliases: []string{"servers"},
		Short:   "Choose which servers a profile includes",
		Long:    "A client bound to the profile sees these servers and nothing else.",
		Args:    cobra.ArbitraryArgs,
		RunE:    groupRunE,
	}
	cmd.AddCommand(a.newProfileServerEditCmd(true), a.newProfileServerEditCmd(false))
	return cmd
}

// newProfileServerEditCmd builds `profile server add|rm`; both edit the
// same three-state list, so they share one implementation.
func (a *App) newProfileServerEditCmd(add bool) *cobra.Command {
	use, short := "rm <profile> <server>", "Take one server out of a profile"
	long := "Affects this profile only; the server stays registered and other profiles keep\n" +
		"it. To take one away from everybody, use 'agenthub server disable'."
	aliases := []string{"remove"}
	if add {
		use, short, aliases = "add <profile> <server>", "Put one server into a profile", nil
		long = "On a fresh profile, which so far includes everything, the first server you add\n" +
			"becomes the only one it includes — so this can narrow what a bound client\n" +
			"sees rather than widen it."
	}
	return &cobra.Command{
		Use:     use,
		Aliases: aliases,
		Short:   short,
		Long:    long,
		Args:    exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			profileName, serverID := args[0], args[1]
			mode := confops.ServerSetRemove
			action := "server removed"
			if add {
				mode, action = confops.ServerSetAdd, "server added"
			}
			store, warnings, err := a.opsStore()
			if err != nil {
				return err
			}
			res, err := confops.SetProfileServers(cmd.Context(), store, profileName,
				confops.ServerSelection{Mode: mode, Servers: []string{serverID}}, noPrecondition)
			warnings = append(warnings, res.Warnings...)
			if err != nil {
				return opsError(err)
			}
			row := profileRow(res.Name, res.Profile, "")
			return a.printer().Emit(ProfileChange{Action: action, Name: profileName, Profile: &row}, warnings...)
		},
	}
}

func (a *App) newProfileToolsCmd() *cobra.Command {
	var (
		only []string
		all  bool
		none bool
	)
	cmd := &cobra.Command{
		Use:   "tools <profile> <server> (--only a,b | --all | --none)",
		Short: "Choose which of one server's tools a profile lets through",
		Long: "Restricts one server, inside one profile, to some of its tools. It does\n" +
			"not change which servers the profile includes.\n\n" +
			"  --only a,b  let just these through\n" +
			"  --none      let none through (the server stays in the profile)\n" +
			"  --all       drop the restriction\n\n" +
			"Use the server's own tool names (search_repositories), not the longer\n" +
			"github__search_repositories your client displays; 'agenthub server test\n" +
			"<server> --tools' lists them. A name the server does not have is stored\n" +
			"anyway — its catalog may simply not have been fetched yet — but it is\n" +
			"reported as a warning, because such a name lets nothing through.",
		Args: exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			profileName, serverID := args[0], args[1]
			sel, err := parseSelectorFlags(cmd, only, all, none)
			if err != nil {
				return err
			}
			store, warnings, err := a.opsStore()
			if err != nil {
				return err
			}
			res, err := confops.SetProfileTools(cmd.Context(), store, profileName, serverID, sel, noPrecondition)
			warnings = append(warnings, res.Warnings...)
			if err != nil {
				return opsError(err)
			}
			// After the write, never before it: the cross-check is advisory,
			// and must not be able to decide whether the rule is stored.
			warnings = append(warnings, a.unknownToolWarning(serverID, sel)...)
			row := profileRow(res.Name, res.Profile, "")
			return a.printer().Emit(ProfileChange{
				Action: "tools updated", Name: profileName, Profile: &row,
			}, warnings...)
		},
	}
	cmd.Flags().StringSliceVar(&only, "only", nil, "let only these tools through, named as the server names them")
	cmd.Flags().BoolVar(&all, "all", false, "drop the restriction: let all of this server's tools through again")
	cmd.Flags().BoolVar(&none, "none", false, "let none of this server's tools through")
	return cmd
}

// discoveryModeLines renders the mode list for `profile discovery --help`,
// marking the default from discovery.DefaultMode rather than in the prose.
//
// The marker was written into the text, on `full`, while the default has been
// lazy for as long as the modes have existed. A help text naming the wrong
// default is worse than one naming none: it is what a reader checks INSTEAD of
// running the command, so the mistake survives every reading of it. Placing
// the marker from the constant means the next change to DefaultMode moves it.
func discoveryModeLines() string {
	lines := []struct {
		mode discovery.Mode
		text string
	}{
		{discovery.ModeFull, "  full     list every tool by name"},
		{discovery.ModeGrouped, "  grouped  list one entry per server, opened up on demand"},
		{discovery.ModeLazy, "  lazy     list a small search-and-call set, and let the client look tools up"},
	}
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.text)
		if l.mode == discovery.DefaultMode {
			b.WriteString(" (the default)")
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (a *App) newProfileDiscoveryCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "discovery <profile> <lazy|grouped|full|->",
		Short: "Choose how a profile's tools are presented to the AI client",
		Long: "Changes how the tools are shown, never which ones get through. Worth changing\n" +
			"only when a profile holds so many that the client's list becomes unwieldy.\n\n" +
			discoveryModeLines() +
			"  -        drop the setting and follow the global default",
		Args: exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			profileName, mode := args[0], args[1]
			if mode == "-" {
				mode = ""
			}
			store, warnings, err := a.opsStore()
			if err != nil {
				return err
			}
			res, err := confops.SetProfileDiscovery(cmd.Context(), store, profileName, mode, noPrecondition)
			warnings = append(warnings, res.Warnings...)
			if err != nil {
				return opsError(err)
			}
			row := profileRow(res.Name, res.Profile, "")
			return a.printer().Emit(ProfileChange{
				Action: "discovery updated", Name: profileName, Profile: &row,
			}, warnings...)
		},
	}
}
