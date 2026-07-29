package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/internal/catalog"
	"github.com/dinstein/agent-hub/internal/confops"
)

// `agenthub catalog` — the curated server directory (docs/modules/controlplane.md).
//
// It exists so that adding a well-known server is a choice from a list
// rather than a remembered `npx -y @modelcontextprotocol/server-…` command
// line, and it exists as a COMMAND because the GUI page has to have one: a
// GUI capability with no CLI equivalent would break the constraint the whole
// control-plane split rests on.
//
// `add` goes through internal/confops like `server add` does — the catalog
// saves the typing, not the validation. It is an OFFLINE command for the
// same reason every other registry write in this package is: the store's
// cross-process lock is what makes concurrent writes safe, and requiring a
// running daemon to add a server would be a new failure mode for no gain.

// CatalogRow is the per-entry data structure both output modes render from.
type CatalogRow struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Publisher   string `json:"publisher,omitempty"`
	Homepage    string `json:"homepage,omitempty"`
	// Provenance grades the SOURCE of the definition (curated / registry /
	// user). It is not a verification claim — nothing here is signed.
	Provenance string   `json:"provenance"`
	Transport  string   `json:"transport"`
	Command    string   `json:"command,omitempty"`
	Args       []string `json:"args,omitempty"`
	URL        string   `json:"url,omitempty"`
	// NeedsConfig is false when `catalog add <id>` needs no further flags.
	NeedsConfig bool `json:"needs_config"`
	// RequiredKeys are the credentials to store afterwards; Params are the
	// values `--param` must supply.
	RequiredKeys []string        `json:"required_keys,omitempty"`
	Params       []catalog.Param `json:"params,omitempty"`
	// Auth is "oauth" when a login is needed after adding.
	Auth string   `json:"auth,omitempty"`
	Tags []string `json:"tags,omitempty"`
}

func catalogRow(e catalog.Entry) CatalogRow {
	return CatalogRow{
		ID:           e.ID,
		Name:         e.Name,
		Description:  e.Description,
		Publisher:    e.Publisher,
		Homepage:     e.Homepage,
		Provenance:   string(e.Provenance),
		Transport:    e.Transport,
		Command:      e.Command,
		Args:         e.Args,
		URL:          e.URL,
		NeedsConfig:  e.NeedsConfig(),
		RequiredKeys: e.RequiredKeys(),
		Params:       e.Params,
		Auth:         e.Auth,
		Tags:         e.Tags,
	}
}

// target is the connection target column: the command line for stdio, the
// endpoint URL otherwise. Same column `server ls` shows, so the two lists
// read the same way.
func (r CatalogRow) target() string {
	if r.URL != "" {
		return r.URL
	}
	return strings.TrimSpace(strings.Join(append([]string{r.Command}, r.Args...), " "))
}

// setup renders what standing between the user and a working server: the
// one-click case says so, everything else names exactly what it wants.
func (r CatalogRow) setup() string {
	if !r.NeedsConfig {
		if r.Auth == catalog.AuthOAuth {
			return "one-click, then login"
		}
		return "one-click"
	}
	var needs []string
	needs = append(needs, r.RequiredKeys...)
	for _, p := range r.Params {
		needs = append(needs, p.Name)
	}
	return "needs " + strings.Join(needs, ", ")
}

// CatalogList is the `catalog ls` / `catalog search` result. JSON shape: a
// plain array, like `server ls`.
type CatalogList []CatalogRow

// Human renders the list as a table.
func (l CatalogList) Human(w io.Writer) error {
	if len(l) == 0 {
		_, err := fmt.Fprintln(w, "no catalog entries match")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tTRANSPORT\tSETUP\tDESCRIPTION")
	for _, r := range l {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.ID, r.Transport, r.setup(), r.Description)
	}
	return tw.Flush()
}

// CatalogEntryView is the `catalog show` result: the whole entry plus the
// command that would add it.
type CatalogEntryView struct {
	CatalogRow
	// AddCommand is the exact invocation, placeholders included. Showing it
	// is the point: the catalog is also how a user learns the CLI.
	AddCommand string `json:"add_command"`
	// NextSteps are the commands that finish the job after the add.
	NextSteps []string `json:"next_steps,omitempty"`
}

// Human renders the detail view.
func (v CatalogEntryView) Human(w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	line := func(k, val string) {
		if val != "" {
			_, _ = fmt.Fprintf(tw, "%s\t%s\n", k, val)
		}
	}
	line("id", v.ID)
	line("name", v.Name)
	line("description", v.Description)
	line("publisher", v.Publisher)
	line("homepage", v.Homepage)
	// Spelled out rather than shown bare: "curated" must not read as
	// "verified" (nothing in the catalog is signed or checked at add time).
	line("provenance", v.Provenance+" (source grading, not a verification)")
	line("transport", v.Transport)
	line("target", v.target())
	line("setup", v.setup())
	for _, p := range v.Params {
		desc := p.Description
		if p.Example != "" {
			desc = strings.TrimSpace(desc + " (e.g. " + p.Example + ")")
		}
		line("param "+p.Name, desc)
	}
	for _, k := range v.RequiredKeys {
		line("credential "+k, "store it with 'agenthub secret set <server> "+k+"'")
	}
	if len(v.Tags) > 0 {
		line("tags", strings.Join(v.Tags, ", "))
	}
	line("add", v.AddCommand)
	for _, s := range v.NextSteps {
		line("then", s)
	}
	return tw.Flush()
}

// CatalogAdded is the `catalog add` result.
type CatalogAdded struct {
	Added ServerRow `json:"added"`
	// CatalogID is where the definition came from (Added.Source carries the
	// same fact as "catalog:<id>").
	CatalogID string `json:"catalog_id"`
	// NextSteps are the commands that make the server actually work.
	NextSteps []string `json:"next_steps,omitempty"`
}

// Human renders the confirmation plus what is left to do. The next steps are
// not decoration: adding a definition is not the same as making a server
// work, and a bare "added" would make a half-done setup look finished.
func (a CatalogAdded) Human(w io.Writer) error {
	if _, err := fmt.Fprintf(w, "added: %s (%s, source=%s)\n",
		a.Added.ID, a.Added.Transport, a.Added.Source); err != nil {
		return err
	}
	for _, s := range a.NextSteps {
		if _, err := fmt.Fprintf(w, "next: %s\n", s); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) newCatalogCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "catalog",
		Short: "Browse the curated MCP server directory and add from it",
		Args:  cobra.ArbitraryArgs,
		RunE:  groupRunE,
	}
	cmd.AddCommand(a.newCatalogLsCmd(), a.newCatalogSearchCmd(),
		a.newCatalogShowCmd(), a.newCatalogAddCmd())
	return cmd
}

func (a *App) newCatalogLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List every curated server",
		Args:  noArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return a.printer().Emit(catalogRows(catalog.List()))
		},
	}
}

func (a *App) newCatalogSearchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "search <query>",
		Short: "Search the curated servers by name, description or tag",
		Args:  rangeArgs(1, 32),
		RunE: func(_ *cobra.Command, args []string) error {
			// Every term must match: `catalog search git hub` narrows.
			return a.printer().Emit(catalogRows(catalog.Search(strings.Join(args, " "))))
		},
	}
}

func (a *App) newCatalogShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show one curated server and the command that adds it",
		Args:  exactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			entry, ok := catalog.Get(args[0])
			if !ok {
				return catalogNotFound(args[0])
			}
			return a.printer().Emit(CatalogEntryView{
				CatalogRow: catalogRow(entry),
				AddCommand: catalogAddCommand(entry),
				NextSteps:  catalogNextSteps(entry, entry.ID),
			})
		},
	}
}

func (a *App) newCatalogAddCmd() *cobra.Command {
	var (
		name     string
		paramsKV []string
	)
	cmd := &cobra.Command{
		Use:   "add <id>",
		Short: "Add a curated server to the registry",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			source, ok := catalog.Get(args[0])
			if !ok {
				return catalogNotFound(args[0])
			}
			params, err := parseKVFlags("--param", paramsKV)
			if err != nil {
				return err
			}
			serverID := name
			if serverID == "" {
				serverID = source.ID
			}
			entry, err := source.Render(params)
			if err != nil {
				return catalogParamError(source, err)
			}
			// Validate BEFORE the store is opened, exactly as `server add`
			// does: a rejected entry must not leave a half-written registry
			// behind. confops re-checks under the lock.
			spec := confops.ServerSpec{ID: serverID, Entry: entry}
			if err := confops.ValidateServerSpec(spec); err != nil {
				return opsError(err)
			}
			store, warnings, err := a.opsStore()
			if err != nil {
				return err
			}
			res, err := confops.AddServer(cmd.Context(), store, spec, noPrecondition)
			warnings = append(warnings, res.Warnings...)
			if err != nil {
				return opsError(err)
			}
			// `catalog add` is an ORCHESTRATION over the two primitives, not
			// a third way to write a server: it composes add + enable so a
			// curated entry that needs nothing else is one command, which is
			// the whole point of the catalog. `server add` stays the
			// configuration-only primitive underneath.
			//
			// An entry that still needs a credential or a login is enabled
			// too — the operator asked for this server by name, and leaving
			// it disabled would mean a second thing to remember on top of
			// the step already named in NextSteps.
			enabled := true
			if _, eerr := confops.SetServerEnabled(cmd.Context(), store, serverID, true, noPrecondition); eerr != nil {
				enabled = false
				warnings = append(warnings,
					"added, but "+serverID+" could not be enabled: "+eerr.Error()+
						"; enable it with 'agenthub server enable "+serverID+"'")
			}
			entry.Enabled = enabled
			return a.printer().Emit(CatalogAdded{
				Added:     rowFor(serverID, entry),
				CatalogID: source.ID,
				NextSteps: catalogNextSteps(source, serverID),
			}, warnings...)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "server name in the registry (default: the catalog id)")
	cmd.Flags().StringArrayVar(&paramsKV, "param", nil,
		"value for a declared parameter, KEY=VALUE (repeatable); see 'agenthub catalog show <id>'")
	return cmd
}

func catalogRows(entries []catalog.Entry) CatalogList {
	out := make(CatalogList, 0, len(entries))
	for _, e := range entries {
		out = append(out, catalogRow(e))
	}
	return out
}

// catalogNotFound is the typed exit-3 failure for an unknown catalog id. It
// reuses CodeNotFound rather than inventing a catalog-specific code: the
// resource classes with their own codes are the ones a script branches on,
// and a catalog id is chosen from a list this same command prints.
func catalogNotFound(id string) error {
	e := NotFoundf(CodeNotFound, "no catalog entry %q", id)
	e.Hint = "list what is available with 'agenthub catalog ls'"
	return e
}

// catalogParamError translates a parameter failure into the CLI's typed
// usage error, with the declared parameters in the hint so the fix travels
// with the refusal.
func catalogParamError(e catalog.Entry, err error) error {
	var perr *catalog.ParamError
	if !errors.As(err, &perr) {
		return err
	}
	out := Usagef("%s", perr.Error())
	out.Hint = "add it with: " + catalogAddCommand(e)
	return out
}

// catalogAddCommand renders the invocation that adds the entry, with one
// --param per declared parameter. This is the string `catalog show` prints
// and the one a GUI would display next to its Add button (docs/modules/controlplane.md).
//
// The parameters are shown as <placeholders>, never as their examples: a
// line the user can copy and run unchanged, with someone else's path in it,
// is worse than a line they must obviously fill in. The example is one row
// above, where it reads as an example.
func catalogAddCommand(e catalog.Entry) string {
	cmd := "agenthub catalog add " + e.ID
	for _, p := range e.Params {
		cmd += fmt.Sprintf(" --param %s=<%s>", p.Name, p.Name)
	}
	return cmd
}

// catalogNextSteps renders what is left after the definition is stored.
// Same list the control plane returns for the same entry — one wording, two
// front ends.
//
// The enable is deliberately NOT listed: `catalog add` performs it, so
// naming it here would tell the operator to run a command that has already
// run. An entry that needs nothing else therefore still has an empty list —
// the one-click case stays one click.
func catalogNextSteps(e catalog.Entry, serverID string) []string {
	var out []string
	for _, key := range e.RequiredKeys() {
		out = append(out, "agenthub secret set "+serverID+" "+key)
	}
	if e.Auth == catalog.AuthOAuth {
		out = append(out, "agenthub auth login "+serverID)
	}
	return out
}
