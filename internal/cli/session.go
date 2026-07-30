package cli

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/api"
	"github.com/dinstein/agent-hub/internal/ctlapi"
	"github.com/dinstein/agent-hub/internal/gateway"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/router"
	"github.com/dinstein/agent-hub/internal/scope"
)

// The `session` group is the runtime view of live connections. Sessions are
// daemon objects that are never persisted, so EVERY subcommand
// here requires the daemon: offline it is exit 4 (E_DAEMON_DOWN), never an
// invented offline answer (docs/modules/controlplane.md online/offline matrix).

// sessionFollowInterval is the `session ls -f` poll period. Polling (not
// SSE) is deliberate: the list is small, and a poll cannot silently stall
// on a half-open stream the way a subscription can.
const sessionFollowInterval = time.Second

// SessionRow is one live session.
type SessionRow struct {
	ID       string `json:"id"`
	ClientID string `json:"client_id"`
	Origin   string `json:"origin"`
	Root     string `json:"root,omitempty"`
	Profile  string `json:"profile_name,omitempty"`
	LastSeen string `json:"last_seen"`
}

// SessionList is the `session ls` result.
type SessionList struct {
	Sessions []SessionRow `json:"sessions"`
}

// Human renders the session table.
func (l SessionList) Human(w io.Writer) error {
	if len(l.Sessions) == 0 {
		_, err := fmt.Fprintln(w, "no live sessions")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "SID\tCLIENT\tORIGIN\tROOT\tPROFILE\tLAST SEEN")
	for _, s := range l.Sessions {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			s.ID, s.ClientID, s.Origin, dash(s.Root), dash(s.Profile), s.LastSeen)
	}
	return tw.Flush()
}

// SessionLayer is one resolved scope layer in `session show`.
type SessionLayer struct {
	Kind      string                           `json:"kind"`
	Origin    string                           `json:"origin"`
	Servers   []string                         `json:"servers"`
	Tools     map[string]registry.ToolSelector `json:"tools,omitempty"`
	Discovery string                           `json:"discovery,omitempty"`
}

// SessionDetail is the `session show` result: the runtime facts from the
// daemon plus the scope chain resolved from the registry.
type SessionDetail struct {
	Session SessionRow     `json:"session"`
	Layers  []SessionLayer `json:"layers"`
	// Effective is the merged visible surface: serverID -> visible ORIGINAL
	// tool names. Empty means the session sees nothing.
	Effective map[string][]string `json:"effective"`
	Discovery string              `json:"discovery,omitempty"`
	// ScopeHash is the content address of the effective scope (the search
	// cache / approval staleness key).
	ScopeHash string `json:"scope_hash"`
	// Diagnostics carry non-fatal resolution warnings — above all a
	// dangling profile reference, which fail-closes to an empty scope and
	// which agenthub reports out loud instead of silently (docs/architecture.md §7).
	Diagnostics []string `json:"diagnostics,omitempty"`
	// Note explains what the layer view does NOT include.
	Note string `json:"note,omitempty"`
}

// Human renders the three-layer view.
func (d SessionDetail) Human(w io.Writer) error {
	if err := (SessionList{Sessions: []SessionRow{d.Session}}).Human(w); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "\nscope layers (least to most specific):"); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "LAYER\tORIGIN\tSERVERS\tDISCOVERY\tTOOL RULES")
	for _, l := range d.Layers {
		rules := "-"
		if len(l.Tools) > 0 {
			parts := make([]string, 0, len(l.Tools))
			for _, id := range sortedKeys(l.Tools) {
				parts = append(parts, id+": "+describeSelector(l.Tools[id]))
			}
			rules = strings.Join(parts, " | ")
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			l.Kind, l.Origin, describeServers(l.Servers), dash(l.Discovery), rules)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "\neffective surface (%s), discovery=%s, hash=%s\n",
		plural(len(d.Effective), "server", "servers"), dash(d.Discovery), d.ScopeHash); err != nil {
		return err
	}
	for _, id := range sortedKeys(d.Effective) {
		if _, err := fmt.Fprintf(w, "  %s: %s\n", id, strings.Join(d.Effective[id], ", ")); err != nil {
			return err
		}
	}
	for _, diag := range d.Diagnostics {
		if _, err := fmt.Fprintf(w, "warning: %s\n", diag); err != nil {
			return err
		}
	}
	if d.Note != "" {
		_, err := fmt.Fprintf(w, "note: %s\n", d.Note)
		return err
	}
	return nil
}

// SessionKillResult is the `session kill` result.
type SessionKillResult struct {
	SessionID string `json:"session_id"`
	ClientID  string `json:"client_id,omitempty"`
	Killed    bool   `json:"killed"`
}

// Human renders the kill confirmation.
func (r SessionKillResult) Human(w io.Writer) error {
	_, err := fmt.Fprintf(w, "killed: session %s (client %s)\n", r.SessionID, dash(r.ClientID))
	return err
}

func (a *App) newSessionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "session",
		Aliases: []string{"sessions"}, // singular canonical, plural alias (canonical.md §3)
		Short:   "Inspect and adjust live sessions (requires a running daemon)",
		Args:    cobra.ArbitraryArgs,
		RunE:    groupRunE,
	}
	cmd.AddCommand(a.newSessionLsCmd(), a.newSessionShowCmd(), a.newSessionKillCmd())
	return cmd
}

func sessionRowOf(in api.SessionInfo) SessionRow {
	return SessionRow{
		ID:       in.ID,
		ClientID: in.ClientID,
		Origin:   in.Origin,
		Root:     in.Root,
		Profile:  in.ProfileName,
		LastSeen: in.LastSeen.UTC().Format(time.RFC3339),
	}
}

// fetchSessions lists live sessions through the control socket, filtered by
// client when asked.
func (a *App) fetchSessions(ctx context.Context, ctl *ctlClient, client string) (SessionList, error) {
	var infos []api.SessionInfo
	if err := ctl.do(ctx, http.MethodGet, "/v1/sessions", nil, &infos); err != nil {
		return SessionList{}, err
	}
	list := SessionList{Sessions: []SessionRow{}}
	for _, in := range infos {
		if client != "" && in.ClientID != client {
			continue
		}
		list.Sessions = append(list.Sessions, sessionRowOf(in))
	}
	return list, nil
}

func (a *App) newSessionLsCmd() *cobra.Command {
	var (
		client string
		follow bool
	)
	cmd := &cobra.Command{
		Use:   "ls [--client x] [-f]",
		Short: "List live sessions (-f re-renders the table as they change)",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctl, _, err := a.requireDaemon(cmd.Context())
			if err != nil {
				return err
			}
			if !follow {
				list, err := a.fetchSessions(cmd.Context(), ctl, client)
				if err != nil {
					return err
				}
				return a.printer().Emit(list)
			}
			return a.followSessions(cmd.Context(), ctl, client)
		},
	}
	cmd.Flags().StringVar(&client, "client", "", "only sessions of this client")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing the list as it changes")
	return cmd
}

// followSessions re-emits the list whenever it changes. Each snapshot is a
// complete envelope, so `-f --json` is a valid NDJSON stream and a consumer
// never has to reassemble partial state.
func (a *App) followSessions(ctx context.Context, ctl *ctlClient, client string) error {
	p := a.printer()
	last := ""
	ticker := time.NewTicker(sessionFollowInterval)
	defer ticker.Stop()
	for {
		list, err := a.fetchSessions(ctx, ctl, client)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		key := fmt.Sprint(list.Sessions)
		if key != last {
			if err := p.Emit(list); err != nil {
				return err
			}
			last = key
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (a *App) newSessionShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <sid>",
		Short: "Show a session's three-layer scope resolution, effective surface and diagnostics",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sid := args[0]
			ctl, _, err := a.requireDaemon(cmd.Context())
			if err != nil {
				return err
			}
			list, err := a.fetchSessions(cmd.Context(), ctl, "")
			if err != nil {
				return err
			}
			var row SessionRow
			found := false
			for _, s := range list.Sessions {
				if s.ID == sid {
					row, found = s, true
					break
				}
			}
			if !found {
				e := NotFoundf(CodeSessionNotFound, "no live session %q", sid)
				e.Hint = "run 'agenthub session ls' to see live sessions"
				return e
			}
			detail, warnings, err := a.resolveSessionScope(row)
			if err != nil {
				return err
			}
			return a.printer().Emit(detail, warnings...)
		},
	}
}

// resolveSessionScope resolves the PERSISTED layers (global and profile) for
// one live session and merges them against the cached catalog.
//
// The session layer — the third and most specific — is deliberately absent:
// the volatile overlay lives in the daemon's memory and is summarized by
// OverlaySummary. Saying so in Note is the point — a view that silently
// omitted it would read as "no overlay" and mislead exactly when it matters.
func (a *App) resolveSessionScope(row SessionRow) (SessionDetail, []string, error) {
	store, warnings, err := a.openStore()
	if err != nil {
		return SessionDetail{}, warnings, err
	}
	snap := store.Snapshot()
	layers, diags := scope.FromRegistry(snap, scope.SessionKey{
		ClientID:  row.ClientID,
		SessionID: scope.SessionID(row.ID),
		Root:      scope.NormalizePath(row.Root),
	})
	cached, err := gateway.LoadToolCache(a.resolver, nil)
	if err != nil {
		return SessionDetail{}, warnings, err
	}
	byServer := make(map[string][]string, len(cached))
	for id, tools := range cached {
		names := make([]string, 0, len(tools))
		for _, t := range tools {
			names = append(names, t.Name)
		}
		byServer[id] = names
	}
	es, err := scope.MergeWithDiagnostics(layers, router.NewCatalog(byServer), diags)
	if err != nil {
		return SessionDetail{}, warnings, fmt.Errorf("resolve scope: %w", err)
	}

	detail := SessionDetail{
		Session:   row,
		Layers:    make([]SessionLayer, 0, len(layers)),
		Effective: map[string][]string{},
		Discovery: string(es.Discovery),
		ScopeHash: hex.EncodeToString(es.Hash[:]),
		Note: "layers cover the four PERSISTED layers; the session overlay is volatile daemon state " +
			"(see the OVERLAY column) and the tool catalog comes from the gateway cache",
	}
	for _, l := range layers {
		sl := SessionLayer{Kind: l.Kind.String(), Origin: l.Origin, Servers: l.Servers}
		if l.Discovery != nil {
			sl.Discovery = string(*l.Discovery)
		}
		if len(l.Tools) > 0 {
			sl.Tools = make(map[string]registry.ToolSelector, len(l.Tools))
			for id, sel := range l.Tools {
				if sel != nil {
					sl.Tools[id] = *sel
				}
			}
		}
		detail.Layers = append(detail.Layers, sl)
	}
	for id, view := range es.Servers {
		detail.Effective[id] = view.Tools
	}
	for _, d := range es.Diags {
		detail.Diagnostics = append(detail.Diagnostics,
			fmt.Sprintf("%s layer (%s): %s", d.Layer, d.Origin, d.Message))
	}
	return detail, warnings, nil
}

func (a *App) newSessionKillCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "kill <sid>",
		Short: "Force-disconnect one live session",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sid := args[0]
			ctl, _, err := a.requireDaemon(cmd.Context())
			if err != nil {
				return err
			}
			var out ctlapi.KillResult
			if err := ctl.do(cmd.Context(), http.MethodPost,
				"/v1/sessions/"+escapePathSegment(sid)+"/kill", struct{}{}, &out); err != nil {
				return classifyScopeError(err)
			}
			return a.printer().Emit(SessionKillResult{
				SessionID: out.SessionID, ClientID: out.ClientID, Killed: out.Killed,
			})
		},
	}
}

// classifyScopeError maps control-plane failures onto the frozen exit-code
// table: an unknown session is exit 3, a refused widening is exit 6
// (governance denial), everything else keeps its transport classification.
func classifyScopeError(err error) error {
	ce, ok := err.(*ctlError)
	if !ok {
		return err
	}
	switch ce.Status {
	case http.StatusNotFound:
		e := NotFoundf(CodeSessionNotFound, "%s", ce.Message)
		e.Hint = "run 'agenthub session ls' to see live sessions"
		return e
	}
	return &Error{Code: ce.Code, ExitCode: ExitGeneral, Message: ce.Message, Hint: ce.Hint}
}

// escapePathSegment percent-escapes one path segment so an id containing a
// slash cannot smuggle extra segments into the request path (the server
// matches on the escaped path for the same reason).
func escapePathSegment(s string) string { return url.PathEscape(s) }

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
