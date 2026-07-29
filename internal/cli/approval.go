package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/api"
	"github.com/dinstein/agent-hub/internal/ctlapi"
)

// ApprovalList is the `approval ls` result.
type ApprovalList struct {
	Approvals []ctlapi.ApprovalWire `json:"approvals"`
	History   bool                  `json:"history"`
}

// Human renders the queue as a table.
func (l ApprovalList) Human(w io.Writer) error {
	if len(l.Approvals) == 0 {
		what := "pending approvals"
		if l.History {
			what = "approvals (pending or recently decided)"
		}
		_, err := fmt.Fprintf(w, "no %s\n", what)
		return err
	}
	for _, ap := range l.Approvals {
		status := ap.Decision
		if status == "" {
			status = "pending, " + remainingText(ap.Deadline)
		} else if ap.DecidedBy != "" {
			status += " by " + ap.DecidedBy
		}
		if _, err := fmt.Fprintf(w, "%s  %s/%s  reason=%s  client=%s  session=%s  [%s]\n",
			ap.Token, ap.Server, ap.Tool, ap.GateReason, ap.Client, ap.SessionID, status); err != nil {
			return err
		}
	}
	return nil
}

// ApprovalDecideResult is the `approval approve|deny` result.
type ApprovalDecideResult struct {
	Token    string `json:"token"`
	Decision string `json:"decision"`
	// AlreadyDecided marks the idempotent 409 path: someone else decided
	// first and nothing was changed by this invocation.
	AlreadyDecided bool   `json:"already_decided,omitempty"`
	Note           string `json:"note,omitempty"`
}

// Human renders the decision outcome.
func (r ApprovalDecideResult) Human(w io.Writer) error {
	if r.AlreadyDecided {
		_, err := fmt.Fprintf(w, "approval %s: %s\n", r.Token, r.Note)
		return err
	}
	if _, err := fmt.Fprintf(w, "approval %s: %s\n", r.Token, r.Decision); err != nil {
		return err
	}
	if r.Note != "" {
		_, err := fmt.Fprintf(w, "note: %s\n", r.Note)
		return err
	}
	return nil
}

func remainingText(deadline time.Time) string {
	left := time.Until(deadline).Round(time.Second)
	if left <= 0 {
		return "expiring"
	}
	return left.String() + " left"
}

// newApprovalCmd builds the approval command group: the `approvals` HITL
// surface, singular canonical + plural alias per canonical.md §3.
func (a *App) newApprovalCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "approval",
		Aliases: []string{"approvals"},
		Short:   "Decide HITL approval requests (any frontend may decide; first answer wins)",
		Args:    cobra.ArbitraryArgs,
		RunE:    groupRunE,
	}
	cmd.AddCommand(
		a.newApprovalLsCmd(),
		a.newApprovalWatchCmd(),
		a.newApprovalDecideCmd(true),
		a.newApprovalDecideCmd(false),
	)
	return cmd
}

func (a *App) newApprovalLsCmd() *cobra.Command {
	var history bool
	cmd := &cobra.Command{
		Use:   "ls [--history]",
		Short: "List pending approval requests (with --history also recent decisions)",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctl, err := a.newCtlClient()
			if err != nil {
				return err
			}
			path := "/v1/approvals"
			if history {
				path += "?history=1"
			}
			list := []ctlapi.ApprovalWire{}
			if err := ctl.do(cmd.Context(), http.MethodGet, path, nil, &list); err != nil {
				return err
			}
			return a.printer().Emit(ApprovalList{Approvals: list, History: history})
		},
	}
	cmd.Flags().BoolVar(&history, "history", false, "include recently decided requests")
	return cmd
}

// newApprovalDecideCmd builds approve or deny (they share everything but
// the verb and the --remember flag).
func (a *App) newApprovalDecideCmd(approve bool) *cobra.Command {
	verb := "deny"
	short := "Deny one approval request by token"
	if approve {
		verb = "approve"
		short = "Approve one approval request by token"
	}
	var remember string
	cmd := &cobra.Command{
		Use:   verb + " <token>",
		Short: short,
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctl, err := a.newCtlClient()
			if err != nil {
				return err
			}
			res, err := decideApproval(cmd.Context(), ctl, args[0], approve, remember)
			if err != nil {
				return err
			}
			return a.printer().Emit(res)
		},
	}
	if approve {
		cmd.Flags().StringVar(&remember, "remember", "",
			`remember this approval: "session" (same session, in memory) or "forever" (fingerprint-bound allowlist)`)
	}
	return cmd
}

// decideApproval posts one decision and classifies the outcome. The 409
// already-decided answer is a SUCCESS for the human ("someone got there
// first"), not an error — the idempotency contract of docs/modules/controlplane.md.
func decideApproval(ctx context.Context, ctl *ctlClient, token string, approve bool, remember string) (ApprovalDecideResult, error) {
	var out ctlapi.ApprovalDecisionWire
	err := ctl.do(ctx, http.MethodPost, "/v1/approvals/"+token,
		ctlapi.ApprovalDecideWire{Approve: approve, Remember: remember}, &out)
	if err == nil {
		return ApprovalDecideResult{Token: token, Decision: out.Decision, Note: out.RememberError}, nil
	}
	var ce *ctlError
	if errors.As(err, &ce) {
		switch ce.Code {
		case ctlapi.CodeAlreadyDecided:
			return ApprovalDecideResult{Token: token, AlreadyDecided: true, Note: ce.Message}, nil
		case ctlapi.CodeNotFound:
			e := NotFoundf(CodeNotFound, "no approval request with token %q", token)
			e.Hint = "see 'agenthub approval ls'"
			return ApprovalDecideResult{}, e
		case ctlapi.CodeExpired:
			return ApprovalDecideResult{}, &Error{
				Code: ctlapi.CodeExpired, ExitCode: ExitGeneral,
				Message: ce.Message,
				Hint:    "the request timed out; re-run the tool call to re-gate it",
			}
		case ctlapi.CodeStale:
			return ApprovalDecideResult{}, &Error{
				Code: ctlapi.CodeStale, ExitCode: ExitDenied,
				Message: ce.Message,
				Hint:    ce.Hint,
			}
		}
	}
	return ApprovalDecideResult{}, err
}

// newApprovalWatchCmd is the line-oriented interactive approval terminal
// (docs/modules/controlplane.md; deliberately no raw-terminal library — plain stdin
// lines). While it runs, it counts as a live approval frontend: gated calls
// reach a human instead of failing Unreachable.
func (a *App) newApprovalWatchCmd() *cobra.Command {
	var notify bool
	cmd := &cobra.Command{
		Use:   "watch [--notify]",
		Short: "Subscribe to approval requests and decide them interactively",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctl, err := a.newCtlClient()
			if err != nil {
				return err
			}
			return a.runApprovalWatch(cmd.Context(), ctl, notify)
		},
	}
	cmd.Flags().BoolVar(&notify, "notify", false, "ring the terminal bell on new requests")
	return cmd
}

// watchState is the pending-request table of one watch session: stable
// human-facing numbers mapped to broker tokens.
type watchState struct {
	next    int
	entries map[int]ctlapi.ApprovalWire // number -> request
	byToken map[string]int
}

func (ws *watchState) add(ap ctlapi.ApprovalWire) (int, bool) {
	if _, dup := ws.byToken[ap.Token]; dup {
		return 0, false // SSE reconnect replays the backlog; keep numbering stable
	}
	ws.next++
	ws.entries[ws.next] = ap
	ws.byToken[ap.Token] = ws.next
	return ws.next, true
}

func (ws *watchState) resolve(token string) (int, bool) {
	n, ok := ws.byToken[token]
	if ok {
		delete(ws.entries, n)
		delete(ws.byToken, token)
	}
	return n, ok
}

// runApprovalWatch pumps SSE approval events and stdin commands until ctx
// ends, the user quits, or the subscription dies. Fail direction on a lost
// daemon: the subscription channel closes only when ctx is done (the api
// client reconnects with backoff), so a daemon restart resumes silently and
// the broker replays the pending queue.
func (a *App) runApprovalWatch(ctx context.Context, ctl *ctlClient, notify bool) error {
	client := api.New(ctl.socket)
	defer client.Close()
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	events, err := client.Events.Subscribe(wctx, "approvals")
	if err != nil {
		var ae *api.Error
		if errors.As(err, &ae) {
			return &Error{Code: CodeGeneral, ExitCode: ExitGeneral, Message: "subscribing to approvals", Err: err}
		}
		return DaemonDownf("daemon is not reachable at %s", ctl.socket)
	}

	if a.jsonOut {
		// JSON mode: a raw event stream, one JSON line per event, no
		// interaction (the envelope convention does not fit an unbounded
		// interactive stream; scripts decide via `approval approve|deny`).
		for ev := range events {
			line, jerr := json.Marshal(ev)
			if jerr != nil {
				continue
			}
			_, _ = fmt.Fprintln(a.stdout, string(line))
		}
		return nil
	}

	_, _ = fmt.Fprintf(a.stdout, "watching approvals on %s\n", ctl.socket)
	_, _ = fmt.Fprintln(a.stdout, `commands: a <n> [session|forever] = approve, d <n> = deny, ls = list, q = quit`)

	// stdin lines. EOF only stops the reader: a headless watch (e.g. spawned
	// as an approval frontend) keeps subscribing without a terminal.
	lines := make(chan string)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(a.stdin)
		for sc.Scan() {
			select {
			case lines <- sc.Text():
			case <-wctx.Done():
				return
			}
		}
	}()

	ws := &watchState{entries: map[int]ctlapi.ApprovalWire{}, byToken: map[string]int{}}
	for {
		select {
		case <-wctx.Done():
			return nil
		case ev, ok := <-events:
			if !ok {
				return nil // subscription ended with ctx
			}
			a.watchEvent(ws, ev, notify)
		case line, ok := <-lines:
			if !ok {
				lines = nil // stdin gone; keep watching
				continue
			}
			quit, derr := a.watchCommand(wctx, ctl, ws, line)
			if derr != nil {
				_, _ = fmt.Fprintf(a.stdout, "error: %v\n", derr)
			}
			if quit {
				return nil
			}
		}
	}
}

// watchEvent renders one SSE event and updates the table.
func (a *App) watchEvent(ws *watchState, ev api.Event, notify bool) {
	switch ev.Kind {
	case "pending":
		var ap ctlapi.ApprovalWire
		if err := json.Unmarshal(ev.Payload, &ap); err != nil {
			return
		}
		n, fresh := ws.add(ap)
		if !fresh {
			return
		}
		bell := ""
		if notify {
			bell = "\a"
		}
		_, _ = fmt.Fprintf(a.stdout, "%s[%d] %s/%s  reason=%s  client=%s  session=%s  token=%s  (%s)\n",
			bell, n, ap.Server, ap.Tool, ap.GateReason, ap.Client, ap.SessionID, ap.Token, remainingText(ap.Deadline))
		if len(ap.Args) > 0 {
			_, _ = fmt.Fprintf(a.stdout, "    args: %s\n", compactJSON(ap.Args))
		}
	case "resolved":
		var res ctlapi.ApprovalResolved
		if err := json.Unmarshal(ev.Payload, &res); err != nil {
			return
		}
		if n, ok := ws.resolve(res.Token); ok {
			_, _ = fmt.Fprintf(a.stdout, "[%d] %s by %s\n", n, res.Decision, res.DecidedBy)
		}
	case "grant_pending", "grant_resolved":
		var g ctlapi.GrantWire
		if err := json.Unmarshal(ev.Payload, &g); err != nil {
			return
		}
		_, _ = fmt.Fprintf(a.stdout, "grant %s: %s  session=%s  %s tools=%s  (see 'agenthub grant ls')\n",
			g.ID, g.Status, g.SessionID, g.Server, strings.Join(g.Tools, ","))
	}
}

// watchCommand executes one interactive line. Returns quit=true for q.
func (a *App) watchCommand(ctx context.Context, ctl *ctlClient, ws *watchState, line string) (bool, error) {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) == 0 {
		return false, nil
	}
	switch fields[0] {
	case "q", "quit", "exit":
		return true, nil
	case "ls":
		if len(ws.entries) == 0 {
			_, _ = fmt.Fprintln(a.stdout, "no pending approvals")
			return false, nil
		}
		for n, ap := range ws.entries {
			_, _ = fmt.Fprintf(a.stdout, "[%d] %s/%s  token=%s  (%s)\n",
				n, ap.Server, ap.Tool, ap.Token, remainingText(ap.Deadline))
		}
		return false, nil
	case "a", "approve", "d", "deny":
		if len(fields) < 2 {
			return false, fmt.Errorf("usage: %s <n> [session|forever]", fields[0])
		}
		n, err := strconv.Atoi(fields[1])
		if err != nil {
			return false, fmt.Errorf("not a request number: %q", fields[1])
		}
		ap, ok := ws.entries[n]
		if !ok {
			return false, fmt.Errorf("no pending request [%d]", n)
		}
		approve := fields[0] == "a" || fields[0] == "approve"
		remember := ""
		if approve && len(fields) > 2 {
			remember = fields[2]
		}
		res, err := decideApproval(ctx, ctl, ap.Token, approve, remember)
		if err != nil {
			return false, err
		}
		switch {
		case res.AlreadyDecided:
			_, _ = fmt.Fprintf(a.stdout, "[%d] %s\n", n, res.Note)
		default:
			_, _ = fmt.Fprintf(a.stdout, "[%d] %s\n", n, res.Decision)
		}
		ws.resolve(ap.Token) // drop locally; the resolved event may race
		return false, nil
	default:
		return false, fmt.Errorf("unknown command %q (a <n> / d <n> / ls / q)", fields[0])
	}
}

// compactJSON renders raw JSON on one line (best effort).
func compactJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}
