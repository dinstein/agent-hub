package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/internal/clients"
	"github.com/dinstein/agent-hub/internal/confops"
	"github.com/dinstein/agent-hub/internal/registry"
)

// `client bind` / `unbind` own clients.json: which profile each agent
// application is on. That is the entire client layer — a client SELECTS a
// profile and never narrows on top of one, so "which profile is this client
// bound to" is a complete answer to "what can it see".
//
// These are PERSISTENT bindings; the volatile per-connection layer is
// `session scope`, which cannot outlive its session.
//
// `client ls` joins that with the OTHER half a user has to hold in their
// head — whether the client is wired up to agenthub at all. That half is not
// in clients.json and deliberately never will be: `connect` writes into the
// client's own configuration file and leaves no record here, so the file is
// the single source of truth. A cached "connected" flag would keep claiming
// a connection a hand edit (or the client rewriting its own config) had
// already removed.
//
// Reading those files is therefore what ls does, and on macOS that can raise
// a privacy prompt. It is the same trade doctor already makes, and it is
// bounded: only clients whose configuration file was found by the stat-only
// scan are opened, so an application that is not installed is never touched.
// `--stat-only` opts out entirely.

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
	// (docs/model.md).
	Dangling bool `json:"dangling,omitempty"`
}

// ClientBindingList is a bare binding listing, kept as the projection
// `client bind` / `unbind` echo back. The overview `client ls` prints is
// ClientList.
type ClientBindingList struct {
	Bindings      []ClientBindingView `json:"bindings"`
	ActiveProfile string              `json:"active_profile"`
	// ActiveDangling marks an active profile that does not exist, so every
	// client following it fail-closes to an empty scope.
	ActiveDangling bool `json:"active_dangling,omitempty"`
}

// Human renders the binding table.
func (l ClientBindingList) Human(w io.Writer) error {
	if len(l.Bindings) == 0 {
		_, err := fmt.Fprintf(w, "no clients bound; every client follows %s\n",
			describeDefaultProfile(l.ActiveProfile, l.ActiveDangling))
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "CLIENT\tPROFILE")
	for _, b := range l.Bindings {
		_, _ = fmt.Fprintf(tw, "%s\t%s\n", b.Client, profileText(b, l.ActiveProfile, l.ActiveDangling))
	}
	return tw.Flush()
}

// ClientRow is one client in the `client ls` overview: is agenthub wired
// into it (connect, which lives in the client's own file), and which profile
// decides what it may see (bind, which lives in clients.json).
type ClientRow struct {
	Client string `json:"client"`
	Name   string `json:"name,omitempty"`

	// State is a clients.ConnectState: connected / not_connected / denied /
	// unreadable / unknown. Connected is only its boolean projection —
	// consumers that treat "not connected" and "could not look" alike are
	// exactly what the other three states exist to prevent.
	State     string `json:"state"`
	Connected bool   `json:"connected"`
	// Read reports whether agenthub opened this client's files at all.
	// False with State=unknown means nobody looked (--stat-only, or a shape
	// agenthub does not parse); false with State=not_connected means the
	// stat already settled it — the client has no configuration file.
	Read bool `json:"read"`
	// Placements names the files that hold the gateway entry.
	Placements []string `json:"placements,omitempty"`
	// ConfigPlacements names the configuration files this client has,
	// connected or not.
	ConfigPlacements []string `json:"config_placements,omitempty"`

	Binding  string `json:"binding"`
	Profile  string `json:"profile,omitempty"`
	Dangling bool   `json:"dangling,omitempty"`
	// EffectiveProfile is the profile that decides this client's scope: its
	// own when bound, the active one when not, "" when neither exists and
	// nothing is narrowed. It saves a consumer the join against
	// active_profile — the join the human table stopped making the reader do.
	EffectiveProfile string `json:"effective_profile,omitempty"`

	Note string `json:"note,omitempty"`
}

// ClientList is the `client ls` result.
type ClientList struct {
	Clients       []ClientRow `json:"clients"`
	ActiveProfile string      `json:"active_profile"`
	// ActiveDangling marks an active profile that does not exist: the
	// unbound clients in this listing fail-close to an empty scope, and the
	// per-row Dangling flag cannot say so — it only covers named bindings.
	ActiveDangling bool `json:"active_dangling,omitempty"`
	// StatOnly records that no configuration file was opened for this
	// listing, so every unknown in it means "not looked at".
	StatOnly bool `json:"stat_only,omitempty"`
}

// Human renders the overview. The NOTE column appears only when a row has
// something to say, which on most machines is never.
func (l ClientList) Human(w io.Writer) error {
	if len(l.Clients) == 0 {
		_, err := fmt.Fprintf(w, "no AI clients found here, and none bound; each would follow %s\n",
			describeDefaultProfile(l.ActiveProfile, l.ActiveDangling))
		return err
	}
	notes := false
	for _, c := range l.Clients {
		if c.Note != "" {
			notes = true
		}
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	header := "CLIENT\tCONNECTED\tWHERE\tPROFILE"
	if notes {
		header += "\tNOTE"
	}
	_, _ = fmt.Fprintln(tw, header)
	for _, c := range l.Clients {
		row := strings.Join([]string{
			c.Client,
			connectedText(c.State),
			joinOrDash(c.Placements),
			// The same "(default)" token `profile ls` heads its table with,
			// pointed at the active profile when there is one. This column
			// used to read "(active)", a name that appeared in no other
			// output — the reader was left to guess it meant the row
			// `profile ls` did not have.
			profileText(ClientBindingView{
				Binding: c.Binding, Profile: c.Profile, Dangling: c.Dangling,
			}, l.ActiveProfile, l.ActiveDangling),
		}, "\t")
		if notes {
			row += "\t" + c.Note
		}
		_, _ = fmt.Fprintln(tw, row)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w,
		"\n%s = the profile a client you have not bound follows: %s\n",
		defaultProfileToken, describeActiveProfile(l.ActiveProfile, l.ActiveDangling))
	return err
}

// describeActiveProfile spells the footer's half of the (default) token: the
// active profile by name, or what an unset one means. The rows print the
// token; this says once what it resolves to, since a client row has no room
// for "every enabled server is visible".
func describeActiveProfile(active string, dangling bool) string {
	switch {
	case active == "":
		return "none set, so every enabled server is visible"
	case dangling:
		return active + ", which does not exist -> those clients see NOTHING " +
			"(fix it with 'agenthub profile use')"
	default:
		return active
	}
}

// connectedText renders a connect state for the table. The states that are
// neither yes nor no keep their own word: "denied" and "unreadable" are
// findings, and "?" is an admission, and printing any of them as "no" would
// send the user to run connect when the fix is elsewhere.
func connectedText(state string) string {
	switch clients.ConnectState(state) {
	case clients.ConnectedYes:
		return "yes"
	case clients.ConnectedNo:
		return "no"
	case clients.ConnectedDenied:
		return "denied"
	case clients.ConnectedUnreadable:
		return "unreadable"
	default:
		return "?"
	}
}

// profileText renders what a client may see: its own profile, or the
// "(default)" it follows when it has none, either of them carrying the loud
// marker when the reference resolves nowhere.
//
// Both halves go through the same marker on purpose. A client bound to a
// missing profile and a client following a missing active profile end up in
// the identical place — an empty scope — and printing one of them quietly
// would leave a whole class of "my client sees no tools" unexplained by this
// table.
func profileText(b ClientBindingView, active string, activeDangling bool) string {
	if b.Binding != string(registry.BindingNamed) {
		return describeDefaultProfile(active, activeDangling)
	}
	if b.Dangling {
		return b.Profile + "  " + missingProfileMarker
	}
	return b.Profile
}

func joinOrDash(v []string) string {
	if len(v) == 0 {
		return "-"
	}
	return strings.Join(v, "+")
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

// effectiveProfileOf names the profile that decides a client's scope: its
// own when bound, the active one when it follows, "" when neither exists.
// A name that resolves nowhere is still returned — it is what the resolver
// looked for, and the dangling flags beside it say it was not found.
func effectiveProfileOf(b ClientBindingView, active string) string {
	if b.Binding == string(registry.BindingNamed) {
		return b.Profile
	}
	return active
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
	var (
		statOnly bool
		all      bool
	)
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "Show which clients agenthub is wired into, and what each one sees",
		Long: "Two answers per client. CONNECTED comes from the client's own config file,\n" +
			"the only place that knows: 'yes' means agenthub's own entry is in it, and\n" +
			"'denied' / 'unreadable' / '?' are never folded into 'no', because they call\n" +
			"for a different fix than running connect. PROFILE is what it may see: its own\n" +
			"profile, or \"(default)\" — the row of the same name in 'agenthub profile ls',\n" +
			"which is whatever 'agenthub profile use' points at. Either of them can carry\n" +
			"MISSING, meaning the profile does not exist and that client sees nothing.\n\n" +
			"Listed are the clients installed here plus any that are bound. Reading their\n" +
			"config files can raise a macOS privacy prompt; only files the scan already\n" +
			"found are opened, and --stat-only opens none at all.",
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
			table := clients.Default()
			base := a.clientBaseDir()

			// The stat-only scan decides which clients are worth opening;
			// an application that is not installed is never touched.
			config := map[string][]string{}
			for _, d := range table.Detect(cmd.Context(), base) {
				config[d.Client] = append(config[d.Client], string(d.Placement))
			}
			ids := map[string]bool{}
			for id := range config {
				ids[id] = true
			}
			// A bound client with nothing on disk stays listed: the binding
			// is a real setting, and a typo'd or uninstalled client that
			// silently vanished from this table would be unfixable from it.
			for id := range snap.Clients.V.Clients {
				ids[id] = true
			}
			if all {
				for _, id := range clients.IDs() {
					ids[id] = true
				}
			}

			list := ClientList{ActiveProfile: active, Clients: []ClientRow{}, StatOnly: statOnly}
			if active != "" {
				if _, ok := snap.Profiles.V.Profiles[active]; !ok {
					// Every unbound row in this listing fail-closes, and no
					// per-row flag covers that case: the row is not bound to
					// anything, so it has nothing of its own to mark.
					list.ActiveDangling = true
					warnings = append(warnings, fmt.Sprintf(
						"active profile %q does not exist -> every client following it resolves to an "+
							"EMPTY scope (fail-closed)", active))
				}
			}
			for _, id := range sortedKeys(ids) {
				b := clientBindingOf(id, snap.Clients.V.Clients[id].V, snap.Profiles.V.Profiles)
				if b.Dangling {
					warnings = append(warnings, fmt.Sprintf(
						"client %q is bound to missing profile %q -> resolves to an EMPTY scope (fail-closed)",
						id, b.Profile))
				}
				row := ClientRow{
					Client: id, Binding: b.Binding, Profile: b.Profile, Dangling: b.Dangling,
					EffectiveProfile: effectiveProfileOf(b, active),
					ConfigPlacements: config[id],
					State:            string(clients.ConnectedUnknown),
				}
				if f, ok := clients.Lookup(id); ok {
					row.Name = f.DisplayName()
				}
				more := a.fillConnectState(&row, table, base, len(config[id]) > 0, statOnly)
				warnings = append(warnings, more...)
				list.Clients = append(list.Clients, row)
			}
			return a.printer().Emit(list, warnings...)
		},
	}
	cmd.Flags().BoolVar(&statOnly, "stat-only", false,
		"do not open any client config file (no privacy prompt); CONNECTED becomes '?'")
	cmd.Flags().BoolVar(&all, "all", false, "list every supported client, installed here or not")
	return cmd
}

// fillConnectState answers the connect half of one row and returns the
// warnings its unreadable files earned.
//
// Failure direction: only the two cases that actually settle the question
// write a definite answer — the stat found no file at all, or every file was
// opened and understood. Everything else keeps the unknown it started with.
func (a *App) fillConnectState(
	row *ClientRow, table *clients.Table, base string, hasFile, statOnly bool,
) []string {
	if !hasFile {
		// Nothing on disk: no entry can be hiding in a file that is not
		// there, and the stat already proved that much.
		row.State = string(clients.ConnectedNo)
		return nil
	}
	if statOnly {
		return nil
	}
	insp, _ := table.Inspect(row.Client, base)
	state, where := insp.ConnectState()
	row.State, row.Connected = string(state), state == clients.ConnectedYes
	for _, p := range where {
		row.Placements = append(row.Placements, string(p))
	}
	var warnings []string
	for _, f := range insp.Files {
		if f.Parsed {
			row.Read = true
		}
		if f.Err != nil {
			warnings = append(warnings, row.Client+": "+f.Error)
		}
	}
	if state == clients.ConnectedUnknown {
		row.Note = "agenthub does not parse this format; check it by hand"
	}
	return warnings
}

func (a *App) newClientBindCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bind <client> <profile>",
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
		Use:   "unbind <id>",
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
