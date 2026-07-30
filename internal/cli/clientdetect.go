package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/internal/clients"
)

// `client detect` is the stat-only inventory of the client group.
//
// detect STATS ONLY (internal/clients.Detect): on macOS, reading another
// application's data directory triggers a TCC privacy prompt, and a bulk
// scan that prompts a dozen times is worse than no scan at all. Reading
// content is reserved for 'client inspect', a deliberate per-client action.

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
	// Supported lists every client agenthub knows about — not the writable
	// subset. That is deliberate: the line exists so "why is my client
	// missing" is answered in the same output, and a codex user asking it
	// needs to find codex in the list.
	//
	// Which is why it may never be LABELLED as the writable set. It was, and
	// it printed codex on the same screen as a row whose WRITABLE column said
	// no; Indirect below is what resolves that, by naming the difference
	// instead of leaving the reader to spot it.
	Supported []string `json:"supported"`
	// Indirect lists the supported clients agenthub does not write itself:
	// the read-only shapes (TOML, YAML, and the fileless remote one), where
	// `client connect` either delegates to the client's own CLI or prints a
	// snippet to paste. Every id here is also in Supported.
	Indirect []string `json:"indirect,omitempty"`
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
	if _, err := fmt.Fprintf(w, "\nsupported clients: %s\n", strings.Join(l.Supported, ", ")); err != nil {
		return err
	}
	if len(l.Indirect) == 0 {
		return nil
	}
	// Named on their own line, because the list above is what a reader
	// compares against the WRITABLE column: without this, the two disagree
	// about the same client and the table is the one that looks wrong.
	_, err := fmt.Fprintf(w,
		"agenthub does not write these itself: %s — 'client connect <id>' says what to do instead\n",
		strings.Join(l.Indirect, ", "))
	return err
}

func (a *App) newClientDetectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "detect",
		Short: "Find the AI clients installed here and where each keeps its config",
		Long: "Start here when you are unsure what to pass to 'agenthub client connect'.\n\n" +
			"It checks only that the config files exist, never opening them: reading every\n" +
			"client's data on sight would set off a macOS privacy prompt per client.\n" +
			"'agenthub client inspect <id>' reads one when you ask it to.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			table := clients.Default()
			found := table.Detect(cmd.Context(), "")
			// Both lists come from the same table the writes go through, so
			// the split cannot drift from what Connect will actually do.
			var indirect []string
			for _, f := range table.Formats() {
				if !f.Writable() {
					indirect = append(indirect, f.ID())
				}
			}
			list := DetectList{
				Found: []DetectedRow{}, Supported: table.IDs(), Indirect: indirect,
			}
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
