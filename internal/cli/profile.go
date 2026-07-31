package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/internal/confops"
	"github.com/dinstein/agent-hub/internal/discovery"
	"github.com/dinstein/agent-hub/internal/gateway"
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
	// Discovery is how this profile's tools are surfaced AS CONFIGURED; ""
	// inherits. It stays the configured value rather than the resolved one:
	// a consumer that read this field and wrote it back would otherwise turn
	// inheritance into a pin.
	Discovery string `json:"discovery,omitempty"`
	// EffectiveDiscovery is the mode this profile's tools are actually
	// surfaced in, and DiscoverySource says which layer decided it
	// (profile / global / builtin).
	EffectiveDiscovery string `json:"effective_discovery,omitempty"`
	DiscoverySource    string `json:"discovery_source,omitempty"`
	// Active marks the globally active profile (`profile use`).
	Active bool `json:"active"`
}

// DefaultProfileRow is the fallback a client with no profile of its own
// follows: the active profile's content, or no narrowing at all while
// `profile use` is unset.
//
// It is NOT a profile, and deliberately does not join ProfileList.Profiles:
// a script walking that array must keep getting names it can hand back to
// `profile rm` / `profile server add`.
type DefaultProfileRow struct {
	// Profile is the active profile this resolves to, "" when none is set.
	Profile string `json:"profile,omitempty"`
	// Dangling marks an active profile that does not exist: every unbound
	// client fail-closes to an empty scope, which is why Servers is then the
	// empty (block-all) list rather than nil.
	Dangling           bool                             `json:"dangling,omitempty"`
	Servers            []string                         `json:"servers"`
	Tools              map[string]registry.ToolSelector `json:"tools,omitempty"`
	EffectiveDiscovery string                           `json:"effective_discovery,omitempty"`
	DiscoverySource    string                           `json:"discovery_source,omitempty"`
}

// ProfileList is the `profile ls` result.
type ProfileList struct {
	Profiles []ProfileRow `json:"profiles"`
	// ActiveProfile is "" when none is set (equivalent to `profile use -`).
	ActiveProfile string `json:"active_profile"`
	// Default is what a client with no binding of its own follows.
	Default DefaultProfileRow `json:"default"`
	// DefaultDiscovery is the mode a profile that sets none inherits, and
	// DefaultDiscoverySource is global (governance.json) or builtin. That is
	// the INHERITANCE target, not Default.EffectiveDiscovery: an active
	// profile may set a mode of its own, and then the two differ.
	DefaultDiscovery       string `json:"default_discovery,omitempty"`
	DefaultDiscoverySource string `json:"default_discovery_source,omitempty"`
}

// describeToolRules renders a profile's per-server selectors for the table.
func describeToolRules(tools map[string]registry.ToolSelector) string {
	if len(tools) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(tools))
	for _, id := range sortedKeys(tools) {
		parts = append(parts, id+": "+describeSelector(tools[id]))
	}
	return strings.Join(parts, " | ")
}

// Human renders the profile table, headed by the (default) row.
//
// That row is always present, including when there are no profiles at all:
// the table's job is to answer "what does a client get", and the answer for
// a client nobody bound was previously nowhere in it — `client ls` printed
// "(active)" and this table had nothing by that name. The row repeats the
// active profile's content rather than pointing at it, so the question is
// answered without a second lookup.
func (l ProfileList) Human(w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tACTIVE\tSERVERS\tDISCOVERY\tTOOL RULES")
	// The star marks the row in force, so exactly one row carries it: the
	// named active profile, or (default) itself when there is none — and
	// also when the active profile does not exist, since the row that would
	// have carried it is precisely the one that is missing.
	defaultActive := ""
	if l.ActiveProfile == "" || l.Default.Dangling {
		defaultActive = "*"
	}
	_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
		describeDefaultProfile(l.Default.Profile, l.Default.Dangling), defaultActive,
		describeServers(l.Default.Servers),
		describeDiscovery(l.Default.EffectiveDiscovery, l.Default.DiscoverySource == discoveryFromProfile),
		describeToolRules(l.Default.Tools))
	for _, p := range l.Profiles {
		active := ""
		if p.Active {
			active = "*"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			p.Name, active, describeServers(p.Servers),
			describeDiscovery(p.EffectiveDiscovery, p.DiscoverySource == discoveryFromProfile),
			describeToolRules(p.Tools))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(l.Profiles) == 0 {
		if _, err := fmt.Fprintln(w,
			"\nno profiles configured; create one with 'agenthub profile create <name>'"); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w,
		"\n%s is what a client with no profile of its own follows; set it with 'agenthub profile use'\n"+
			"inherited discovery: %s (%s)\n",
		defaultProfileToken, dash(l.DefaultDiscovery), describeDiscoverySource(l.DefaultDiscoverySource))
	return err
}

// describeDiscoverySource spells out where an inherited mode came from, so
// the reader knows which knob moves it.
func describeDiscoverySource(source string) string {
	if source == discoveryFromGlobal {
		return "governance.json, from 'agenthub config set discovery'"
	}
	return "built in; 'agenthub config set discovery' overrides it"
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
		a.newProfileToolCmd(),
		a.newProfileDiscoveryCmd(),
	)
	return cmd
}

// profileRow projects one registry profile into its rendered form. The
// snapshot resolves the effective discovery mode and may be nil, which drops
// those two fields rather than guessing them.
func profileRow(name string, p registry.Profile, active string, snap *registry.Snapshot) ProfileRow {
	row := ProfileRow{
		Name: name, Servers: p.Servers, Discovery: p.Discovery,
		Active: name == active && active != "",
	}
	if snap != nil {
		row.EffectiveDiscovery, row.DiscoverySource = discoveryOf(snap, name)
	}
	if len(p.Tools) > 0 {
		row.Tools = make(map[string]registry.ToolSelector, len(p.Tools))
		for id, doc := range p.Tools {
			row.Tools[id] = doc.V
		}
	}
	return row
}

// defaultProfileRow resolves what an unbound client follows: the active
// profile's own content, or no narrowing at all when none is set.
//
// A dangling active profile resolves to the EMPTY server list, not to nil:
// that is what the scope chain does with it (fail-closed, block-all), and a
// table that showed "(all registered)" there would describe the opposite of
// what those clients get.
func defaultProfileRow(snap *registry.Snapshot, active string) DefaultProfileRow {
	row := DefaultProfileRow{Profile: active}
	row.EffectiveDiscovery, row.DiscoverySource = discoveryOf(snap, active)
	if active == "" {
		return row
	}
	doc, ok := snap.Profiles.V.Profiles[active]
	if !ok {
		row.Dangling = true
		row.Servers = []string{}
		return row
	}
	row.Servers = doc.V.Servers
	if len(doc.V.Tools) > 0 {
		row.Tools = make(map[string]registry.ToolSelector, len(doc.V.Tools))
		for id, sel := range doc.V.Tools {
			row.Tools[id] = sel.V
		}
	}
	return row
}

func (a *App) newProfileLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List your profiles and what each one includes",
		Long: "The first row, \"(default)\", is not a profile: it is what a client you have not\n" +
			"bound follows — the active profile, or every enabled server while none is set.\n" +
			"It is the row 'agenthub client ls' points at, and the star marks whichever row\n" +
			"is in force.\n\n" +
			"In the servers column, \"(all registered)\" means the profile takes nothing away\n" +
			"and \"(none)\" means a client bound to it sees nothing. The discovery column is\n" +
			"the mode that will actually be used; \"(inherited)\" means the profile does not\n" +
			"set one and the global default decides. For which client is on which profile,\n" +
			"use 'agenthub client ls'.",
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
				list.Profiles = append(list.Profiles, profileRow(name, snap.Profiles.V.Profiles[name].V, active, snap))
			}
			list.DefaultDiscovery, list.DefaultDiscoverySource = discoveryOf(snap, "")
			list.Default = defaultProfileRow(snap, active)
			if list.Default.Dangling {
				warnings = append(warnings, fmt.Sprintf(
					"active profile %q does not exist; every followActive binding fail-closes to an empty scope", active))
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
			row := profileRow(res.Name, res.Profile, "", store.Snapshot())
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
			row := profileRow(res.Name, res.Profile, res.Name, store.Snapshot())
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
			row := profileRow(res.Name, res.Profile, "", store.Snapshot())
			return a.printer().Emit(ProfileChange{Action: action, Name: profileName, Profile: &row}, warnings...)
		},
	}
}

// newProfileToolCmd is the profile half of the tool model, and it is named to
// match the global half: `profile tool ls | allow` and `server tool ls | allow`
// are one mechanism at two altitudes, so they are one spelling at two
// altitudes.
//
// The reading half was missing here for a release, and its absence had a
// shape: `server tool ls` said what the machine offered, `profile ls` said
// what each profile narrowed, and the INTERSECTION — the only thing a client
// bound to that profile actually gets — was left for the reader to work out
// per tool, in their head. That is the arithmetic `server tool inspect` exists
// to stop them doing, and nothing was doing it at listing granularity.
func (a *App) newProfileToolCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "tool",
		Aliases: []string{"tools"},
		Short:   "See and narrow the tools a profile lets through",
		Args:    cobra.ArbitraryArgs,
		RunE:    groupRunE,
	}
	cmd.AddCommand(a.newProfileToolLsCmd(), a.newProfileToolInspectCmd(), a.newProfileToolAllowCmd())
	return cmd
}

func (a *App) newProfileToolLsCmd() *cobra.Command {
	var (
		search  string
		showAll bool
	)
	cmd := &cobra.Command{
		Use:   "ls <profile> [<server>]",
		Short: "List the tools this profile lets through",
		Long: "What a client bound to this profile ends up with: the machine-wide allow\n" +
			"lists and the profile's own narrowing, intersected — which is what the\n" +
			"gateway hands out, rather than either layer read on its own.\n\n" +
			"--all adds the tools that were held back, each with the layer that took it:\n" +
			"'global' is 'agenthub server tool allow', 'profile:servers' means the profile\n" +
			"does not include that server ('agenthub profile server add' puts it back), and\n" +
			"'profile:tools' is this profile's own allow list. The three need different\n" +
			"repairs, which is why the listing says which one applied.\n\n" +
			"'agenthub server tool ls' is the same reading one layer up: what the machine\n" +
			"offers before any profile narrows it.",
		Args: rangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			profileName, serverArg := args[0], ""
			if len(args) == 2 {
				serverArg = args[1]
			}
			store, warnings, err := a.openStore()
			if err != nil {
				return err
			}
			snap := store.Snapshot()
			// A profile that does not exist is an ERROR here, not the empty
			// scope it fail-closes to at runtime. A session has to keep going
			// and must not widen; a reader who mistyped a name is not served
			// by a correct listing of nothing.
			if _, ok := snap.Profiles.V.Profiles[profileName]; !ok {
				e := NotFoundf(CodeProfileNotFound, "no profile %q", profileName)
				e.Hint = "run 'agenthub profile ls' to see the profiles you have"
				return e
			}
			if serverArg != "" {
				if _, ok := snap.Servers.V.Servers[serverArg]; !ok {
					e := NotFoundf(CodeServerNotFound, "no server %q", serverArg)
					e.Hint = "run 'agenthub server ls' to see configured servers"
					return e
				}
			}
			cached, err := gateway.LoadToolCache(a.resolver, nil)
			if err != nil {
				return err
			}
			list, err := toolListing(snap, cached, toolListingRequest{
				server: serverArg, profile: profileName, search: search, showAll: showAll,
			})
			if err != nil {
				return err
			}
			return a.printer().Emit(list, warnings...)
		},
	}
	cmd.Flags().StringVar(&search, "search", "", "rank the tools against a keyword query")
	cmd.Flags().BoolVar(&showAll, "all", false,
		"list the tools a layer holds back too, with the state of each and the layer that took it")
	return cmd
}

func (a *App) newProfileToolAllowCmd() *cobra.Command {
	var (
		only []string
		all  bool
		none bool
	)
	cmd := &cobra.Command{
		Use:   "allow <profile> <server> (--only a,b | --all | --none)",
		Short: "Choose which of one server's tools a profile lets through",
		Long: "Restricts one server, inside one profile, to some of its tools. It does\n" +
			"not change which servers the profile includes.\n\n" +
			"  --only a,b  let just these through\n" +
			"  --none      let none through (the server stays in the profile)\n" +
			"  --all       drop the restriction\n\n" +
			"Use the server's own tool names (search_repositories), not the longer\n" +
			"github__search_repositories your client displays; 'agenthub server tool ls\n" +
			"<server>' lists them. A name the server does not have is stored anyway —\n" +
			"its catalog may simply not have been fetched yet — but it is reported as a\n" +
			"warning, because such a name lets nothing through.\n\n" +
			"'agenthub server tool allow' is this same edit one layer up, with the same\n" +
			"flags and the same names: it decides what the machine offers at all, this\n" +
			"decides what a profile passes on. The two intersect; neither can widen.",
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
			row := profileRow(res.Name, res.Profile, "", store.Snapshot())
			return a.printer().Emit(ProfileChange{
				Action: "tools updated", Name: profileName, Profile: &row,
			}, warnings...)
		},
	}
	addSelectorFlags(cmd, &only, &all, &none,
		"let only these tools through, named as the server names them",
		"drop the restriction: let all of this server's tools through again",
		"let none of this server's tools through")
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
			row := profileRow(res.Name, res.Profile, "", store.Snapshot())
			return a.printer().Emit(ProfileChange{
				Action: "discovery updated", Name: profileName, Profile: &row,
			}, warnings...)
		},
	}
}
