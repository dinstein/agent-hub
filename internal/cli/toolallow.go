package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/internal/confops"
)

// `tool allow` is the global half of the tool model: which of a server's
// tools this machine offers at all, before any profile narrows it further.
//
// It is an ALLOW list, not a kill switch, and the difference shows up on the
// day the downstream adds a tool. Under an allow list the new tool is not
// exposed until someone says so; under a deny list it is live the moment the
// server ships it. Only one of those is a decision the operator actually
// made, so there is only one verb here.
//
// It writes the registry, not a state file: the rule lives on the server
// entry beside `enabled`, and a running gateway adopts it through the
// registry watch that already exists.

// ToolAllowResult is the `tool allow` result.
type ToolAllowResult struct {
	Server string `json:"server"`
	// Tools is the allow list now in force: null = every tool the server
	// offers, [] = none, [...] = exactly those. The null/empty distinction
	// is the whole answer, so it is never omitted.
	Tools []string `json:"tools"`
}

// Human renders the resulting rule, spelling out the two states a reader
// would otherwise have to infer from an absent line.
func (r ToolAllowResult) Human(w io.Writer) error {
	switch {
	case r.Tools == nil:
		_, err := fmt.Fprintf(w, "%s: every tool the server offers\n", r.Server)
		return err
	case len(r.Tools) == 0:
		_, err := fmt.Fprintf(w, "%s: no tools (the server is registered but exposes nothing)\n", r.Server)
		return err
	default:
		_, err := fmt.Fprintf(w, "%s: %s\n", r.Server, strings.Join(r.Tools, ", "))
		return err
	}
}

// newToolAllowCmd builds `tool allow <server> [tool...]`.
func (a *App) newToolAllowCmd() *cobra.Command {
	var clear bool
	cmd := &cobra.Command{
		Use:   "allow <server> [tool...] [--clear]",
		Short: "Choose which of a server's tools are offered at all",
		Long: "Names the tools this server contributes, for every client at once. A profile\n" +
			"can narrow the result further; nothing can widen it.\n\n" +
			"With no rule a server offers everything it has, including tools it gains\n" +
			"later. Naming tools here fixes the set: a tool the server adds afterwards\n" +
			"stays out until you add it.\n\n" +
			"  tool allow github get_issue list_prs   offer exactly those two\n" +
			"  tool allow github                      offer nothing from this server\n" +
			"  tool allow github --clear              go back to offering everything",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			server, tools := args[0], args[1:]
			if clear && len(tools) > 0 {
				return Usagef("--clear takes no tool names: it removes the rule entirely")
			}
			store, warnings, err := a.opsStore()
			if err != nil {
				return err
			}
			// nil clears the rule; a non-nil empty slice is "offer nothing",
			// and the two must not be conflated on the way down.
			var sel []string
			if !clear {
				sel = []string{}
				sel = append(sel, tools...)
			}
			res, err := confops.SetServerTools(cmd.Context(), store, server, sel, noPrecondition)
			warnings = append(warnings, res.Warnings...)
			if err != nil {
				return opsError(err)
			}
			out := ToolAllowResult{Server: server}
			if len(res.Servers) > 0 {
				out.Tools = res.Servers[0].Entry.Tools
			}
			return a.printer().Emit(out, warnings...)
		},
	}
	cmd.Flags().BoolVar(&clear, "clear", false,
		"remove the rule, going back to offering every tool the server has")
	return cmd
}
