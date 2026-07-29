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
	"github.com/dinstein/agent-hub/internal/confops"
	"github.com/dinstein/agent-hub/internal/ctlapi"
	"github.com/dinstein/agent-hub/internal/gateway"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/router"
	"github.com/dinstein/agent-hub/internal/scope"
)

// The `session` group is the runtime view of live connections. Sessions are
// daemon objects that are never persisted (A.1 #6), so EVERY subcommand
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
	Overlay  string `json:"overlay_summary,omitempty"`
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
	_, _ = fmt.Fprintln(tw, "SID\tCLIENT\tORIGIN\tROOT\tPROFILE\tOVERLAY\tLAST SEEN")
	for _, s := range l.Sessions {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			s.ID, s.ClientID, s.Origin, dash(s.Root), dash(s.Profile), dash(s.Overlay), s.LastSeen)
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

// Human renders the four-layer view.
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

// SessionScopeResult is the `session scope` result.
type SessionScopeResult struct {
	SessionID string `json:"session_id"`
	// Applied lists the runtime overlay edits that took effect.
	Applied []string `json:"applied,omitempty"`
	// Persisted names the client whose clients.json entry was amended
	// (--persist), "" when the change stayed volatile.
	Persisted string `json:"persisted,omitempty"`
	// GrantID is set when a widening request was filed instead of applied:
	// agents and operators may only NARROW at runtime; widening is a human
	// grant with a TTL (A.1 #8).
	GrantID string `json:"grant_id,omitempty"`
	Note    string `json:"note,omitempty"`
}

// Human renders the outcome.
func (r SessionScopeResult) Human(w io.Writer) error {
	for _, s := range r.Applied {
		if _, err := fmt.Fprintf(w, "session %s: %s\n", r.SessionID, s); err != nil {
			return err
		}
	}
	if r.Persisted != "" {
		if _, err := fmt.Fprintf(w, "persisted to clients.json#%s\n", r.Persisted); err != nil {
			return err
		}
	}
	if r.GrantID != "" {
		if _, err := fmt.Fprintf(w, "widen request filed: grant %s (decide with 'agenthub grant approve %s')\n",
			r.GrantID, r.GrantID); err != nil {
			return err
		}
	}
	if r.Note != "" {
		_, err := fmt.Fprintf(w, "note: %s\n", r.Note)
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
	cmd.AddCommand(a.newSessionLsCmd(), a.newSessionShowCmd(), a.newSessionScopeCmd(), a.newSessionKillCmd())
	return cmd
}

func sessionRowOf(in api.SessionInfo) SessionRow {
	return SessionRow{
		ID:       in.ID,
		ClientID: in.ClientID,
		Origin:   in.Origin,
		Root:     in.Root,
		Profile:  in.ProfileName,
		Overlay:  in.OverlaySummary,
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
		Short: "Show a session's four-layer scope resolution, effective surface and diagnostics",
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

// resolveSessionScope resolves the PERSISTED four layers for one live
// session and merges them against the cached catalog.
//
// The session (fifth) layer is deliberately absent: the volatile overlay
// lives in the daemon's memory and is summarized by OverlaySummary. Saying
// so in Note is the point — a view that silently omitted it would read as
// "no overlay" and mislead exactly when it matters.
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

// sessionScopeFlags groups the frozen session-level flags (canonical.md §3).
type sessionScopeFlags struct {
	enableServer  []string
	disableServer []string
	tools         []string
	discovery     string
	reset         bool
	persist       bool
	ttl           time.Duration
	reason        string
}

func (a *App) newSessionScopeCmd() *cobra.Command {
	var f sessionScopeFlags
	cmd := &cobra.Command{
		Use: "scope <sid> [--disable-server s] [--tools s:t1,t2] [--discovery m] " +
			"[--enable-server s] [--reset] [--persist]",
		Short: "Adjust a live session's scope (narrowing is applied; widening files a grant)",
		Long: "Adjust one live session's scope overlay.\n\n" +
			"Narrowing (--disable-server, --tools, --reset) and the experience field\n" +
			"--discovery are applied straight to the volatile overlay.\n\n" +
			"--enable-server WIDENS, which no runtime path may do on its own (A.1 #8):\n" +
			"it files a TTL-bounded grant that a human approves with 'agenthub grant\n" +
			"approve'. Name the tools to widen with --tools <server>:<tool>,...\n\n" +
			"--persist additionally writes the narrowing into the client layer\n" +
			"(clients.json), where it survives the session.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runSessionScope(cmd, args[0], f)
		},
	}
	cmd.Flags().StringArrayVar(&f.enableServer, "enable-server", nil,
		"file a widen request for this server (needs --tools <server>:<tool>,...)")
	cmd.Flags().StringArrayVar(&f.disableServer, "disable-server", nil, "hide this server from the session")
	cmd.Flags().StringArrayVar(&f.tools, "tools", nil, "narrow one server's tools: <server>:<tool>[,<tool>] (repeatable)")
	cmd.Flags().StringVar(&f.discovery, "discovery", "", "discovery mode for this session: lazy, grouped or full")
	cmd.Flags().BoolVar(&f.reset, "reset", false, "drop the overlay and restore the static scope baseline")
	cmd.Flags().BoolVar(&f.persist, "persist", false, "also write the narrowing into the client layer (clients.json)")
	cmd.Flags().DurationVar(&f.ttl, "ttl", 0, "widen-request lifetime after approval (default 1h)")
	cmd.Flags().StringVar(&f.reason, "reason", "", "reason recorded on a widen request")
	return cmd
}

func (a *App) runSessionScope(cmd *cobra.Command, sid string, f sessionScopeFlags) error {
	if len(f.enableServer) == 0 && len(f.disableServer) == 0 && len(f.tools) == 0 &&
		f.discovery == "" && !f.reset {
		e := Usagef("nothing to change: pass --enable-server/--disable-server/--tools/--discovery/--reset")
		e.Hint = helpHint(cmd)
		return e
	}
	if f.discovery != "" {
		if err := validateDiscovery(f.discovery); err != nil {
			return err
		}
	}
	toolSpecs, err := parseToolSpecs(f.tools)
	if err != nil {
		return err
	}
	ctl, _, err := a.requireDaemon(cmd.Context())
	if err != nil {
		return err
	}

	res := SessionScopeResult{SessionID: sid}
	// Widening first: if a widen request is refused there is no point
	// applying the narrowings that were meant to accompany it.
	if len(f.enableServer) > 0 {
		id, err := a.fileWidenGrant(cmd, ctl, sid, f, toolSpecs)
		if err != nil {
			return err
		}
		res.GrantID = id
	}

	if body, ok := scopeNarrowBody(f, toolSpecs); ok {
		if err := ctl.do(cmd.Context(), http.MethodPost,
			"/v1/sessions/"+escapePathSegment(sid)+"/scope", body, nil); err != nil {
			return classifyScopeError(err)
		}
		res.Applied = describeScopeEdits(body)
	}

	if f.persist {
		client, err := a.persistSessionScope(cmd.Context(), ctl, sid, f, toolSpecs)
		if err != nil {
			return err
		}
		res.Persisted = client
	}
	if res.GrantID == "" && len(res.Applied) == 0 && res.Persisted == "" {
		res.Note = "nothing changed"
	}
	return a.printer().Emit(res)
}

// scopeNarrowBody builds the narrowing request from the flags, reporting
// ok=false when they amount to no narrowing at all — which is not the same as
// "no narrowing flags were passed": --tools naming only servers that
// --enable-server also names leaves nothing behind once the filter below runs.
//
// That filter is the subtle part. A tool spec for a server being WIDENED is
// the widen request's payload — the list of tools the grant should open. Sent
// here it would mean the opposite edit: restrict that server to exactly those
// tools, applied immediately and without the approval the widen path exists to
// require. Narrowing and widening share one --tools flag, so this is where the
// two readings are separated.
func scopeNarrowBody(f sessionScopeFlags, toolSpecs map[string][]string) (ctlapi.ScopeNarrowWire, bool) {
	// DisableServers and Reset live on the embedded api.ScopeNarrow, so they
	// cannot be set in the composite literal.
	body := ctlapi.ScopeNarrowWire{Discovery: f.discovery}
	body.DisableServers = f.disableServer
	body.Reset = f.reset
	for id, tools := range toolSpecs {
		if containsString(f.enableServer, id) {
			continue
		}
		if body.Tools == nil {
			body.Tools = map[string][]string{}
		}
		body.Tools[id] = tools
	}
	ok := body.Reset || len(body.DisableServers) > 0 || len(body.Tools) > 0 || body.Discovery != ""
	return body, ok
}

// fileWidenGrant turns --enable-server into a pending grant. Tools must be
// named explicitly: the daemon's undo of an expired grant is element-wise,
// which is what makes the revert provably tightening (ctlapi/grants.go).
func (a *App) fileWidenGrant(
	cmd *cobra.Command, ctl *ctlClient, sid string,
	f sessionScopeFlags, toolSpecs map[string][]string,
) (string, error) {
	if len(f.enableServer) > 1 {
		e := Usagef("--enable-server takes one server per invocation (each widen request is decided on its own)")
		e.Hint = helpHint(cmd)
		return "", e
	}
	server := f.enableServer[0]
	tools := toolSpecs[server]
	if len(tools) == 0 {
		e := Usagef("--enable-server %s needs the tools to widen: --tools %s:<tool>[,<tool>]", server, server)
		e.Hint = helpHint(cmd)
		return "", e
	}
	var out ctlapi.GrantWire
	err := ctl.do(cmd.Context(), http.MethodPost, "/v1/grants", ctlapi.GrantRequestWire{
		SessionID:  sid,
		Server:     server,
		Tools:      tools,
		Reason:     f.reason,
		TTLSeconds: int64(f.ttl / time.Second),
	}, &out)
	if err != nil {
		return "", classifyScopeError(err)
	}
	return out.ID, nil
}

// persistSessionScope writes the session's narrowing into the CLIENT layer,
// so it outlives the session. Only narrowings are persisted: a widening is
// a TTL-bounded grant by construction and must never become permanent
// configuration behind the operator's back.
func (a *App) persistSessionScope(
	ctx context.Context, ctl *ctlClient, sid string,
	f sessionScopeFlags, toolSpecs map[string][]string,
) (string, error) {
	list, err := a.fetchSessions(ctx, ctl, "")
	if err != nil {
		return "", err
	}
	client := ""
	for _, s := range list.Sessions {
		if s.ID == sid {
			client = s.ClientID
			break
		}
	}
	if client == "" {
		e := NotFoundf(CodeSessionNotFound, "no live session %q", sid)
		e.Hint = "run 'agenthub session ls' to see live sessions"
		return "", e
	}
	_, err = a.mutate(ctx, func(tx *registry.Tx) error {
		if tx.Clients.V.Clients == nil {
			tx.Clients.V.Clients = map[string]registry.Doc[registry.ClientEntry]{}
		}
		doc := tx.Clients.V.Clients[client]
		entry := doc.V
		if entry.Tools == nil {
			entry.Tools = map[string]registry.Doc[registry.ToolSelector]{}
		}
		for _, id := range f.disableServer {
			confops.ApplyToolSelection(entry.Tools, id, confops.ToolSelection{Mode: confops.ToolSelectNone})
		}
		for _, id := range sortedKeys(toolSpecs) {
			if containsString(f.enableServer, id) {
				continue
			}
			confops.ApplyToolSelection(entry.Tools, id, toolSelectionFor(toolSpecs[id]))
		}
		if f.discovery != "" {
			entry.Discovery = f.discovery
		}
		if len(entry.Tools) == 0 {
			entry.Tools = nil
		}
		doc.V = entry
		tx.Clients.V.Clients[client] = doc
		return nil
	})
	if err != nil {
		return "", err
	}
	return client, nil
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

// describeScopeEdits renders what was applied, from the very body that was
// sent (one source, so the report can never claim an edit that was not
// actually requested).
func describeScopeEdits(body ctlapi.ScopeNarrowWire) []string {
	var out []string
	if body.Reset {
		out = append(out, "overlay reset to the static baseline")
	}
	for _, id := range body.DisableServers {
		out = append(out, "server "+id+" hidden")
	}
	for _, id := range sortedKeys(body.Tools) {
		if len(body.Tools[id]) == 0 {
			out = append(out, "server "+id+" tools blocked")
			continue
		}
		out = append(out, "server "+id+" narrowed to "+strings.Join(body.Tools[id], ","))
	}
	if body.Discovery != "" {
		out = append(out, "discovery set to "+body.Discovery)
	}
	return out
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
	case http.StatusForbidden:
		return &Error{
			Code: CodeTightenOnly, ExitCode: ExitDenied,
			Message: ce.Message,
			Hint:    "runtime scope may only narrow; widen with 'agenthub session scope --enable-server' (files a grant)",
		}
	}
	return &Error{Code: ce.Code, ExitCode: ExitGeneral, Message: ce.Message, Hint: ce.Hint}
}

// escapePathSegment percent-escapes one path segment so an id containing a
// slash cannot smuggle extra segments into the request path (the server
// matches on the escaped path for the same reason).
func escapePathSegment(s string) string { return url.PathEscape(s) }

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
