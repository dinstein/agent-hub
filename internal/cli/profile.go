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
	_, _ = fmt.Fprintln(tw, "NAME\tACTIVE\tSERVERS\tTOOL RULES")
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
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", p.Name, active, describeServers(p.Servers), rules)
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
		Short:   "Manage capability profiles (server sets + three-state tool selectors)",
		Args:    cobra.ArbitraryArgs,
		RunE:    groupRunE,
	}
	cmd.AddCommand(
		a.newProfileLsCmd(),
		a.newProfileCreateCmd(),
		a.newProfileRmCmd(),
		a.newProfileRenameCmd(),
		a.newProfileUseCmd(),
		a.newProfileServerCmd(),
		a.newProfileToolsCmd(),
	)
	return cmd
}

// profileRow projects one registry profile into its rendered form.
func profileRow(name string, p registry.Profile, active string) ProfileRow {
	row := ProfileRow{Name: name, Servers: p.Servers, Active: name == active && active != ""}
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
		Short: "List profiles (* marks the active one)",
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
		Short: "Create an empty profile (no narrowing until servers or tool rules are added)",
		Args:  exactArgs(1),
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
		"initial server set (omit for 'no narrowing'; pass an empty value for block-all)")
	return cmd
}

func (a *App) newProfileRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "rm <name>",
		Aliases: []string{"remove"},
		Short:   "Remove a profile",
		Args:    exactArgs(1),
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
		Short: "Rename a profile and repoint every client that references it",
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
		Short: `Set the globally active profile ("-" clears it, back to every registered server)`,
		Args:  exactArgs(1),
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
		Short:   "Add or remove a server from a profile's server set",
		Args:    cobra.ArbitraryArgs,
		RunE:    groupRunE,
	}
	cmd.AddCommand(a.newProfileServerEditCmd(true), a.newProfileServerEditCmd(false))
	return cmd
}

// newProfileServerEditCmd builds `profile server add|rm`; both edit the
// same three-state list, so they share one implementation.
func (a *App) newProfileServerEditCmd(add bool) *cobra.Command {
	use, short := "rm <profile> <server-id>", "Remove a server from a profile's server set"
	aliases := []string{"remove"}
	if add {
		use, short, aliases = "add <profile> <server-id>", "Add a server to a profile's server set", nil
	}
	return &cobra.Command{
		Use:     use,
		Aliases: aliases,
		Short:   short,
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
		Short: "Set a profile's three-state tool selector for one server",
		Long: "Set the tool selector of one server inside a profile.\n\n" +
			"Three states (docs/architecture.md §7, keyed by ORIGINAL tool names):\n" +
			"  --all       the server's full tool set (removes the rule)\n" +
			"  --only a,b  narrow to that subset\n" +
			"  --none      block every tool of the server (fail-closed)",
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
			row := profileRow(res.Name, res.Profile, "")
			return a.printer().Emit(ProfileChange{
				Action: "tools updated", Name: profileName, Profile: &row,
			}, warnings...)
		},
	}
	cmd.Flags().StringSliceVar(&only, "only", nil, "narrow to these original tool names")
	cmd.Flags().BoolVar(&all, "all", false, "expose the server's full tool set")
	cmd.Flags().BoolVar(&none, "none", false, "block every tool of the server")
	return cmd
}
