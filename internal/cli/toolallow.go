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
// IT IS `profile tools`, ONE LAYER UP. Both narrow one server's tools, both
// are allow lists over the server's OWN tool names, both take
// --only/--all/--none through parseSelectorFlags, both cross-check spelling
// through unknownToolWarning, and both resolve the three states through one
// confops.allowList. The only difference is altitude: this one answers for
// every client on the machine, that one for the clients bound to a profile,
// and the two intersect. Anything learned about one transfers — which is the
// point, because an operator holding two mental models for one mechanism
// eventually applies the fail-open one.
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

// newToolAllowCmd builds `tool allow <server> (--only … | --all | --none)`.
//
// The flag trio replaced a positional tool list, and the missing form is the
// reason: `tool allow github` with the names forgotten used to mean "expose
// NOTHING from github". It was one slip away from the opposite of the intent,
// it was silent — the command reported success and no listing showed the rule
// — and it was spelled differently from the same edit one layer up. Requiring
// one of the three costs a flag and removes all three faults.
func (a *App) newToolAllowCmd() *cobra.Command {
	var (
		only []string
		all  bool
		none bool
	)
	cmd := &cobra.Command{
		Use:   "allow <server> (--only a,b | --all | --none)",
		Short: "Choose which of a server's tools are offered at all",
		Long: "Names the tools this server contributes, for every client at once. A profile\n" +
			"can narrow the result further; nothing can widen it.\n\n" +
			"  --only a,b  offer exactly these\n" +
			"  --none      offer nothing from this server (it stays registered)\n" +
			"  --all       drop the rule, offering everything again\n\n" +
			"With no rule a server offers everything it has, including tools it gains\n" +
			"later. --only fixes the set: a tool the server adds afterwards stays out\n" +
			"until you add it.\n\n" +
			"Use the server's own tool names (get_issue), not the longer github__get_issue\n" +
			"your client displays; 'agenthub tool ls <server>' lists them. Same flags, same\n" +
			"names and same allow-list semantics as 'agenthub profile tools', which applies\n" +
			"the next layer down.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			server := args[0]
			sel, err := parseSelectorFlags(cmd, only, all, none)
			if err != nil {
				return err
			}
			store, warnings, err := a.opsStore()
			if err != nil {
				return err
			}
			res, err := confops.SetServerTools(cmd.Context(), store, server, sel, noPrecondition)
			warnings = append(warnings, res.Warnings...)
			if err != nil {
				return opsError(err)
			}
			// After the write, never before it: the cross-check is advisory,
			// and must not be able to decide whether the rule is stored.
			warnings = append(warnings, a.unknownToolWarning(server, sel)...)
			out := ToolAllowResult{Server: server}
			if len(res.Servers) > 0 {
				out.Tools = res.Servers[0].Entry.Tools
			}
			return a.printer().Emit(out, warnings...)
		},
	}
	cmd.Flags().StringSliceVar(&only, "only", nil, "offer only these tools, named as the server names them")
	cmd.Flags().BoolVar(&all, "all", false, "drop the rule: offer every tool the server has again")
	cmd.Flags().BoolVar(&none, "none", false, "offer none of this server's tools")
	return cmd
}
