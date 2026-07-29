package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/internal/clients"
	"github.com/dinstein/agent-hub/internal/registry"
)

// `client detect` and `client import` complete the client group.
//
// detect STATS ONLY (internal/clients.Detect): on macOS, reading another
// application's data directory triggers a TCC privacy prompt, and a bulk
// scan that prompts a dozen times is worse than no scan at all. Reading
// content is reserved for import, which is a deliberate per-client action.

// DetectedRow is one discovered client configuration file.
type DetectedRow struct {
	Client    string `json:"client"`
	Name      string `json:"name"`
	Placement string `json:"placement"`
	Shape     string `json:"shape"`
	Path      string `json:"path"`
	Writable  bool   `json:"writable"`
	Size      int64  `json:"size"`
	Modified  string `json:"modified"`
	Note      string `json:"note,omitempty"`
	// Denied marks a location that exists but may not be inspected.
	// "You may not look" is a finding; "it is not there" is not.
	Denied      bool   `json:"denied,omitempty"`
	Remediation string `json:"remediation,omitempty"`
}

// DetectList is the `client detect` result.
type DetectList struct {
	Found []DetectedRow `json:"found"`
	// Supported lists every client agenthub can write directly, so the
	// answer to "why is my client missing" is in the same output.
	Supported []string `json:"supported"`
}

// Human renders the detection table.
func (l DetectList) Human(w io.Writer) error {
	if len(l.Found) == 0 {
		if _, err := fmt.Fprintln(w, "no client configurations found on this machine"); err != nil {
			return err
		}
	} else {
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "CLIENT\tPLACEMENT\tWRITABLE\tSIZE\tMODIFIED\tPATH")
		for _, d := range l.Found {
			path := d.Path
			if d.Denied {
				path += "  (permission denied)"
			}
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\n",
				d.Client, d.Placement, boolText(d.Writable), d.Size, d.Modified, path)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "\ndirectly writable clients: %s\n", strings.Join(l.Supported, ", "))
	return err
}

// ImportedServer is one entry an import added to the registry.
type ImportedServer struct {
	Name         string `json:"name"`
	Transport    string `json:"transport"`
	Source       string `json:"source"`
	OriginalName string `json:"original_name,omitempty"`
}

// ImportResult is the `client import` result.
type ImportResult struct {
	Client string   `json:"client"`
	DryRun bool     `json:"dry_run"`
	Files  []string `json:"files"`
	// Added lists what was (or would be) written into the registry.
	Added []ImportedServer `json:"added"`
	// Conflicts names entries whose target already exists. They are NEVER
	// imported: an import must not silently redefine a governed server.
	Conflicts []string `json:"conflicts,omitempty"`
	// Skipped names unconvertible entries with a reason.
	Skipped []string `json:"skipped,omitempty"`
	// SecretWarnings names entries that look like they carry a literal
	// credential; the registry must never hold one.
	SecretWarnings []string `json:"secret_warnings,omitempty"`
	// AsTeam records that --as-team was requested. Team scoping lands in
	// M2; the flag is accepted now and reported so an operator learns it
	// was a no-op instead of assuming it worked.
	AsTeam bool `json:"as_team,omitempty"`
}

// Human renders the import outcome.
func (r ImportResult) Human(w io.Writer) error {
	verb := "imported"
	if r.DryRun {
		verb = "would import"
	}
	if len(r.Added) == 0 {
		if _, err := fmt.Fprintf(w, "nothing to import from %s\n", r.Client); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintf(w, "%s %s from %s:\n",
			verb, plural(len(r.Added), "server", "servers"), r.Client); err != nil {
			return err
		}
		for _, s := range r.Added {
			renamed := ""
			if s.OriginalName != "" && s.OriginalName != s.Name {
				renamed = fmt.Sprintf(" (renamed from %q)", s.OriginalName)
			}
			if _, err := fmt.Fprintf(w, "  %s (%s, source=%s)%s\n",
				s.Name, s.Transport, s.Source, renamed); err != nil {
				return err
			}
		}
	}
	for _, c := range r.Conflicts {
		if _, err := fmt.Fprintf(w, "conflict (kept existing): %s\n", c); err != nil {
			return err
		}
	}
	for _, s := range r.Skipped {
		if _, err := fmt.Fprintf(w, "skipped: %s\n", s); err != nil {
			return err
		}
	}
	for _, s := range r.SecretWarnings {
		if _, err := fmt.Fprintf(w,
			"warning: %s looks like it carries a literal credential; move it with 'agenthub secret set'\n",
			s); err != nil {
			return err
		}
	}
	if r.AsTeam {
		_, err := fmt.Fprintln(w, "note: --as-team is recorded only; team scoping arrives in M2")
		return err
	}
	return nil
}

func (a *App) newClientDetectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "detect",
		Short: "Find the AI clients installed here and where each keeps its config",
		Long: "Start here when you are unsure what to pass to 'agenthub client connect'.\n\n" +
			"It checks only that the config files exist, never opening them: reading every\n" +
			"client's data on sight would set off a macOS privacy prompt per client.\n" +
			"'agenthub client import' reads one when you ask it to.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			table := clients.Default()
			found := table.Detect(cmd.Context(), "")
			list := DetectList{Found: []DetectedRow{}, Supported: clients.IDs()}
			var warnings []string
			for _, d := range found {
				row := DetectedRow{
					Client: d.Client, Name: d.Name, Placement: string(d.Placement),
					Shape: string(d.Shape), Path: d.Path, Writable: d.Writable,
					Size: d.Size, Note: d.Note, Denied: d.Denied, Remediation: d.Remediation,
				}
				if !d.Modified.IsZero() {
					row.Modified = d.Modified.UTC().Format(time.RFC3339)
				}
				if d.Denied {
					warnings = append(warnings, fmt.Sprintf("%s: %s is not readable: %s",
						d.Client, d.Path, d.Remediation))
				}
				list.Found = append(list.Found, row)
			}
			return a.printer().Emit(list, warnings...)
		},
	}
}

func (a *App) newClientImportCmd() *cobra.Command {
	var (
		asTeam bool
		dryRun bool
	)
	cmd := &cobra.Command{
		Use:   "import <client> [--as-team]",
		Short: "Take over the MCP servers a client is already configured with",
		Long: "Saves re-entering servers you already set up in that client. Try --dry-run\n" +
			"first. A name clashing with a server you already have is reported and\n" +
			"skipped; an import never overwrites one you set up yourself.\n\n" +
			"This copies the servers over — it does not wire the client up to agenthub or\n" +
			"remove its old entries. Use 'agenthub client connect' for that.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			clientID := args[0]
			store, warnings, err := a.openStore()
			if err != nil {
				return err
			}
			existing := sortedKeys(store.Snapshot().Servers.V.Servers)
			res, err := clients.Import(clientID, existing)
			if err != nil {
				return classifyImportError(err, clientID)
			}

			out := ImportResult{
				Client: clientID, DryRun: dryRun, Files: res.Sources,
				Added: []ImportedServer{}, AsTeam: asTeam,
			}
			for _, name := range sortedKeys(res.Entries) {
				entry := res.Entries[name]
				out.Added = append(out.Added, ImportedServer{
					Name: name, Transport: entry.TransportName(), Source: entry.Source,
					OriginalName: res.Renamed[name],
				})
			}
			for _, c := range res.Conflicts {
				out.Conflicts = append(out.Conflicts, fmt.Sprintf("%s (%s)", c.Name, c.Path))
			}
			for _, s := range res.Skipped {
				out.Skipped = append(out.Skipped, fmt.Sprintf("%s: %s", s.Name, s.Reason))
			}
			out.SecretWarnings = res.SecretWarnings
			if dryRun || len(res.Entries) == 0 {
				return a.printer().Emit(out, warnings...)
			}

			more, err := a.mutate(cmd.Context(), func(tx *registry.Tx) error {
				if tx.Servers.V.Servers == nil {
					tx.Servers.V.Servers = map[string]registry.Doc[registry.ServerEntry]{}
				}
				for name, entry := range res.Entries {
					// Re-check under the lock: the snapshot that produced the
					// conflict list is older than this transaction, and an
					// import must never overwrite an entry that appeared in
					// between.
					if _, exists := tx.Servers.V.Servers[name]; exists {
						out.Conflicts = append(out.Conflicts, name+" (added concurrently)")
						continue
					}
					tx.Servers.V.Servers[name] = registry.Doc[registry.ServerEntry]{V: entry}
				}
				return nil
			})
			warnings = append(warnings, more...)
			if err != nil {
				return err
			}
			return a.printer().Emit(out, warnings...)
		},
	}
	cmd.Flags().BoolVar(&asTeam, "as-team", false, "note these as shared with a team; nothing behaves differently yet")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would be imported without changing anything")
	return cmd
}

// classifyImportError maps internal/clients failures onto the exit-code
// table: an unknown client id is exit 3, an unsupported (unparsed) config
// format is a plain failure with the manual snippet as the hint.
func classifyImportError(err error, clientID string) error {
	var unknown *clients.UnknownClientError
	if errors.As(err, &unknown) {
		e := NotFoundf(CodeClientUnsupported, "unknown client %q", clientID)
		e.Hint = "known clients: " + strings.Join(clients.IDs(), ", ")
		return e
	}
	var unsupported *clients.UnsupportedError
	if errors.As(err, &unsupported) {
		return &Error{
			Code: CodeClientUnsupported, ExitCode: ExitGeneral,
			Message: unsupported.Error(),
			Hint:    "agenthub only reads configuration formats it can parse; add the servers manually",
			Err:     err,
		}
	}
	return classifyClientsError(err)
}
