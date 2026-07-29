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
