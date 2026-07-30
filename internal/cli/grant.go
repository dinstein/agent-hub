package cli

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/internal/ctlapi"
)

// GrantList is the `grant ls` result.
type GrantList struct {
	Grants  []ctlapi.GrantWire `json:"grants"`
	History bool               `json:"history"`
}

// Human renders the grant table.
func (l GrantList) Human(w io.Writer) error {
	if len(l.Grants) == 0 {
		_, err := fmt.Fprintln(w, "no grants")
		return err
	}
	for _, g := range l.Grants {
		status := g.Status
		if g.Status == ctlapi.GrantApproved && g.ExpiresAt != nil {
			status += ", expires in " + time.Until(*g.ExpiresAt).Round(time.Second).String()
		} else if g.DecidedBy != "" {
			status += " by " + g.DecidedBy
		}
		reason := g.Reason
		if reason == "" {
			reason = "-"
		}
		if _, err := fmt.Fprintf(w, "%s  session=%s  %s tools=%s  ttl=%s  reason=%s  [%s]\n",
			g.ID, g.SessionID, g.Server, strings.Join(g.Tools, ","),
			(time.Duration(g.TTLSeconds) * time.Second).String(), reason, status); err != nil {
			return err
		}
	}
	return nil
}

// GrantResult is the create/approve/deny result: the grant as the daemon
// now sees it.
type GrantResult struct {
	Grant ctlapi.GrantWire `json:"grant"`
	// AlreadyDecided marks the idempotent 409 path.
	AlreadyDecided bool   `json:"already_decided,omitempty"`
	Note           string `json:"note,omitempty"`
}

// Human renders the outcome.
func (r GrantResult) Human(w io.Writer) error {
	if r.AlreadyDecided {
		_, err := fmt.Fprintf(w, "grant %s: %s\n", r.Grant.ID, r.Note)
		return err
	}
	g := r.Grant
	if _, err := fmt.Fprintf(w, "grant %s: %s  session=%s  %s tools=%s\n",
		g.ID, g.Status, g.SessionID, g.Server, strings.Join(g.Tools, ",")); err != nil {
		return err
	}
	if g.Status == ctlapi.GrantApproved && g.ExpiresAt != nil {
		_, err := fmt.Fprintf(w, "widening active until %s (volatile: dies with the session or daemon)\n",
			g.ExpiresAt.Format(time.RFC3339))
		return err
	}
	return nil
}

// newGrantCmd builds the grant command group: the human-approved TEMPORARY
// widening path of A.1 #8 (agents may only narrow on their own; a widen
// request must pass a human and expires on a TTL).
func (a *App) newGrantCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "grant",
		Aliases: []string{"grants"},
		Short:   "Review session scope-widen requests (human-approved, TTL-bounded, never persisted)",
		Args:    cobra.ArbitraryArgs,
		RunE:    groupRunE,
	}
	cmd.AddCommand(
		a.newGrantLsCmd(),
		a.newGrantDecideCmd(true),
		a.newGrantDecideCmd(false),
		a.newGrantRequestCmd(),
	)
	return cmd
}

func (a *App) newGrantRequestCmd() *cobra.Command {
	var (
		sessionID string
		server    string
		tools     []string
		reason    string
		ttl       time.Duration
	)
	cmd := &cobra.Command{
		Use:   "request --session <id> --server <id> --tool <name> [--tool ...]",
		Short: "File a widen request on behalf of a live session (you file it, not the agent)",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if sessionID == "" || server == "" || len(tools) == 0 {
				e := Usagef("--session, --server and at least one --tool are required")
				e.Hint = helpHint(cmd)
				return e
			}
			ctl, err := a.newCtlClient()
			if err != nil {
				return err
			}
			var out ctlapi.GrantWire
			err = ctl.do(cmd.Context(), http.MethodPost, "/v1/grants", ctlapi.GrantRequestWire{
				SessionID:  sessionID,
				Server:     server,
				Tools:      tools,
				Reason:     reason,
				TTLSeconds: int64(ttl / time.Second),
			}, &out)
			if err != nil {
				return classifyGrantErr(err, "session "+sessionID)
			}
			return a.printer().Emit(GrantResult{Grant: out})
		},
	}
	cmd.Flags().StringVar(&sessionID, "session", "", "target session id (see 'agenthub session ls' / daemon status)")
	cmd.Flags().StringVar(&server, "server", "", "downstream server whose narrowing to relax")
	cmd.Flags().StringArrayVar(&tools, "tool", nil, "raw tool name to widen (repeatable)")
	cmd.Flags().StringVar(&reason, "reason", "", "why the widening is needed (shown to the approver)")
	cmd.Flags().DurationVar(&ttl, "ttl", 0, "widening lifetime after approval (default 1h, max 24h)")
	return cmd
}

func (a *App) newGrantLsCmd() *cobra.Command {
	var history bool
	cmd := &cobra.Command{
		Use:   "ls [--history]",
		Short: "List pending and active grants (with --history also recent decisions)",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctl, err := a.newCtlClient()
			if err != nil {
				return err
			}
			path := "/v1/grants"
			if history {
				path += "?history=1"
			}
			list := []ctlapi.GrantWire{}
			if err := ctl.do(cmd.Context(), http.MethodGet, path, nil, &list); err != nil {
				return err
			}
			return a.printer().Emit(GrantList{Grants: list, History: history})
		},
	}
	cmd.Flags().BoolVar(&history, "history", false, "include recently decided grants")
	return cmd
}

func (a *App) newGrantDecideCmd(approve bool) *cobra.Command {
	verb, short := "deny", "Deny one widen request by id"
	if approve {
		verb, short = "approve", "Approve one widen request (injects the widening into the session overlay)"
	}
	return &cobra.Command{
		Use:   verb + " <id>",
		Short: short,
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctl, err := a.newCtlClient()
			if err != nil {
				return err
			}
			var out ctlapi.GrantWire
			err = ctl.do(cmd.Context(), http.MethodPost, "/v1/grants/"+args[0],
				ctlapi.GrantDecideWire{Approve: approve}, &out)
			if err != nil {
				var ce *ctlError
				if errors.As(err, &ce) && ce.Code == ctlapi.CodeAlreadyDecided {
					// Idempotent: first decision won; report, exit 0.
					return a.printer().Emit(GrantResult{
						Grant:          ctlapi.GrantWire{ID: args[0]},
						AlreadyDecided: true,
						Note:           ce.Message,
					})
				}
				return classifyGrantErr(err, "grant "+args[0])
			}
			return a.printer().Emit(GrantResult{Grant: out})
		},
	}
}

// classifyGrantErr maps daemon error envelopes onto the typed CLI table.
func classifyGrantErr(err error, what string) error {
	var ce *ctlError
	if errors.As(err, &ce) && ce.Code == ctlapi.CodeNotFound {
		e := NotFoundf(CodeNotFound, "%s not found", what)
		e.Hint = "see 'agenthub grant ls' (a grant target session must be live)"
		return e
	}
	return err
}
