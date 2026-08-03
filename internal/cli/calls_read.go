package cli

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/internal/calllog"
	"github.com/dinstein/agent-hub/internal/secrets"
)

// CallEventRow is the metadata-only projection used by audit tail.
type CallEventRow struct {
	Time          time.Time `json:"time"`
	CallID        string    `json:"callId"`
	Event         string    `json:"event"`
	Client        string    `json:"client,omitempty"`
	Face          string    `json:"face,omitempty"`
	ExposedTool   string    `json:"exposedTool,omitempty"`
	Server        string    `json:"server,omitempty"`
	Tool          string    `json:"tool,omitempty"`
	Outcome       string    `json:"outcome,omitempty"`
	DurationMs    int64     `json:"durationMs,omitempty"`
	Code          string    `json:"code,omitempty"`
	ResultCapture string    `json:"resultCapture,omitempty"`
}

func auditEventRow(e calllog.Event) CallEventRow {
	return CallEventRow{
		Time: e.TS, CallID: e.CallID, Event: string(e.Kind), Client: e.Client,
		Face: e.Face, ExposedTool: e.Exposed, Server: e.Server, Tool: e.Tool,
		Outcome: e.Outcome, DurationMs: e.DurationMs, Code: e.Code,
		ResultCapture: e.ResultCapture,
	}
}

// AuditTail is a bounded recent-event view. Payloads are never decrypted.
type AuditTail struct {
	Since   time.Time      `json:"since,omitempty"`
	Events  []CallEventRow `json:"events"`
	Skipped int            `json:"skippedMalformed"`
}

func (t AuditTail) Human(w io.Writer) error {
	if len(t.Events) == 0 {
		_, err := fmt.Fprintln(w, "no matching call events")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "TIME\tCALL\tEVENT\tCLIENT\tTARGET\tOUTCOME\tCODE")
	for _, e := range t.Events {
		target := e.ExposedTool
		if e.Server != "" || e.Tool != "" {
			target = e.Server + "/" + e.Tool
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			e.Time.Local().Format(time.RFC3339), shortCallID(e.CallID), e.Event,
			dash(e.Client), dash(target), dash(e.Outcome), dash(e.Code))
	}
	return tw.Flush()
}

func shortCallID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// callsFollowInterval is the -f re-read period. It matches the event and
// frame readers': the three are read side by side, and one period keeps a
// record from surfacing in one view before another.
const callsFollowInterval = 500 * time.Millisecond

// callSelector is the narrowing shared by the one-shot read and the follower,
// so -f cannot drift into admitting something a plain tail would not.
type callSelector struct {
	client, server, tool, outcome string
}

func (s callSelector) admit(e calllog.Event) bool {
	return auditEventMatches(e, s.client, s.server, s.tool, s.outcome)
}

// readCallTail collects the newest `limit` matching events since `since`.
// A limit of zero or less is unbounded, which is what the follower asks for:
// it has already narrowed the window with a cursor, and a ring buffer on top
// of that could only throw away records it was about to print.
func readCallTail(root string, since time.Time, limit int, sel callSelector) (AuditTail, error) {
	rows := make([]CallEventRow, 0, max(limit, 0))
	skipped, err := calllog.ScanEventsSince(root, since, func(e calllog.Event) error {
		if !sel.admit(e) {
			return nil
		}
		if limit > 0 && len(rows) == limit {
			copy(rows, rows[1:])
			rows = rows[:limit-1]
		}
		rows = append(rows, auditEventRow(e))
		return nil
	})
	if err != nil {
		return AuditTail{}, err
	}
	return AuditTail{Since: since, Events: rows, Skipped: skipped}, nil
}

// followCalls prints every event recorded after the tail it was given.
//
// It tracks a record TIMESTAMP rather than a byte offset, for the reason
// followEvents does: the ledger is a directory of day-partitioned files
// written by N gateway processes, so an offset into "the file" is not a
// position in the stream at all.
//
// The cursor is the record's own time.Time, taken from the row before it is
// rendered — NOT parsed back out of the printed stamp. followEvents explains
// why at length: a rendered RFC3339 stamp is second resolution, so a cursor
// read back from one advances a whole second and silently drops the rest of
// that second's records. followServerFrames does exactly that today and is
// the one follower of the four that still gets this wrong; copy this loop
// rather than that one.
//
// The cursor is the newest record PRINTED, and admission is strictly after
// it. A record sharing that instant is the one case this loses, and the
// alternative — >= — reprints the last row on every tick, which reads as the
// same call happening over and over.
func (a *App) followCalls(ctx context.Context, root string, seen AuditTail, sel callSelector) error {
	p := a.printer()
	cursor := seen.Since
	if n := len(seen.Events); n > 0 {
		cursor = seen.Events[n-1].Time
	}
	ticker := time.NewTicker(callsFollowInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		// Re-read from the cursor rather than from `since`: a long-running
		// follower would otherwise re-scan a widening window every tick.
		batch, err := readCallTail(root, cursor, 0, sel)
		if err != nil {
			return err
		}
		fresh := AuditTail{}
		for _, row := range batch.Events {
			if !row.Time.After(cursor) {
				continue
			}
			fresh.Events = append(fresh.Events, row)
		}
		if len(fresh.Events) == 0 {
			continue
		}
		cursor = fresh.Events[len(fresh.Events)-1].Time
		if err := p.Emit(fresh); err != nil {
			return err
		}
	}
}

func (a *App) newCallsTailCmd() *cobra.Command {
	var (
		sinceRaw string
		limit    int
		client   string
		server   string
		tool     string
		outcome  string
		follow   bool
	)
	cmd := &cobra.Command{
		Use:   "tail [-f]",
		Short: "Show recent call metadata without decrypting payloads",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if limit <= 0 || limit > 1000 {
				return Usagef("--limit must be from 1 through 1000")
			}
			since, err := parseAuditSince(sinceRaw)
			if err != nil {
				return err
			}
			root, err := calllog.DefaultDir(a.resolver)
			if err != nil {
				return err
			}
			sel := callSelector{client: client, server: server, tool: tool, outcome: outcome}
			tail, err := readCallTail(root, since, limit, sel)
			if err != nil {
				return err
			}
			if err := a.printer().Emit(tail); err != nil {
				return err
			}
			if !follow {
				return nil
			}
			return a.followCalls(cmd.Context(), root, tail, sel)
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "stay open and keep printing new events")
	cmd.Flags().StringVar(&sinceRaw, "since", "24h", "look back by duration or RFC3339 time; use all for no time bound")
	cmd.Flags().IntVar(&limit, "limit", 20, "maximum events to return (1-1000)")
	cmd.Flags().StringVar(&client, "client", "", "only this client")
	cmd.Flags().StringVar(&server, "server", "", "only this routed server")
	cmd.Flags().StringVar(&tool, "tool", "", "only this raw or exposed tool")
	cmd.Flags().StringVar(&outcome, "outcome", "", "only this outcome")
	return cmd
}

func parseAuditSince(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "all" {
		return time.Time{}, nil
	}
	if d, err := time.ParseDuration(raw); err == nil {
		if d <= 0 {
			return time.Time{}, Usagef("--since duration must be positive")
		}
		return time.Now().Add(-d), nil
	}
	if ts, err := time.Parse(time.RFC3339, raw); err == nil {
		return ts, nil
	}
	return time.Time{}, Usagef("--since expects a duration such as 24h, RFC3339 time, or all")
}

func auditEventMatches(e calllog.Event, client, server, tool, outcome string) bool {
	return (client == "" || e.Client == client) &&
		(server == "" || e.Server == server) &&
		(tool == "" || e.Tool == tool || e.Exposed == tool) &&
		(outcome == "" || e.Outcome == outcome)
}

// AuditCall is one call's whole story: the lifecycle at the client boundary,
// the frames at the downstream one, and — with --payloads — the bodies.
//
// The frames are what the ledger could not show before they moved into it. A
// call that retried twice has one `routed` record and three sent/recv pairs,
// and reading the two halves as one sequence is the only way that reads as
// what happened rather than as a slow call with no explanation.
type AuditCall struct {
	CallID             string          `json:"callId"`
	Events             []calllog.Event `json:"events"`
	Request            string          `json:"request,omitempty"`
	EffectiveArguments string          `json:"effectiveArguments,omitempty"`
	Result             string          `json:"result,omitempty"`
	// Frames holds the decrypted frame bodies in event order, one per
	// sent/recv record that had one.
	Frames []string `json:"frames,omitempty"`
}

func (c AuditCall) Human(w io.Writer) error {
	_, _ = fmt.Fprintf(w, "call: %s\n", c.CallID)
	for _, e := range c.Events {
		_, _ = fmt.Fprintf(w, "%s  %-8s", e.TS.Local().Format(time.RFC3339Nano), e.Kind)
		if e.Exposed != "" {
			_, _ = fmt.Fprintf(w, "  exposed=%s", e.Exposed)
		}
		if e.Server != "" || e.Tool != "" {
			_, _ = fmt.Fprintf(w, "  target=%s/%s", e.Server, e.Tool)
		}
		if e.Method != "" {
			_, _ = fmt.Fprintf(w, "  method=%s", e.Method)
		}
		if e.Seq > 1 {
			// Only from the second attempt: printing "attempt 1" on every
			// frame of a call that never retried is noise that hides the one
			// case the number exists for.
			_, _ = fmt.Fprintf(w, "  attempt=%d", e.Seq)
		}
		if e.DurationMs > 0 {
			_, _ = fmt.Fprintf(w, "  %dms", e.DurationMs)
		}
		if e.Outcome != "" {
			_, _ = fmt.Fprintf(w, "  outcome=%s", e.Outcome)
		}
		if e.Code != "" {
			_, _ = fmt.Fprintf(w, "  code=%s", e.Code)
		}
		_, _ = fmt.Fprintln(w)
	}
	if c.Request != "" {
		_, _ = fmt.Fprintf(w, "\nrequest:\n%s\n", c.Request)
	}
	if c.EffectiveArguments != "" {
		_, _ = fmt.Fprintf(w, "\neffective arguments:\n%s\n", c.EffectiveArguments)
	}
	if c.Result != "" {
		_, _ = fmt.Fprintf(w, "\nresult capture:\n%s\n", c.Result)
	}
	for i, f := range c.Frames {
		_, _ = fmt.Fprintf(w, "\nframe %d:\n%s\n", i+1, f)
	}
	return nil
}

func (a *App) newCallsShowCmd() *cobra.Command {
	var payloads bool
	cmd := &cobra.Command{
		Use:   "show <call-id>",
		Short: "Show one call lifecycle; decrypt payloads only with --payloads",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := calllog.DefaultDir(a.resolver)
			if err != nil {
				return err
			}
			var events []calllog.Event
			collect := func(e calllog.Event) error {
				if e.CallID == args[0] {
					events = append(events, e)
				}
				return nil
			}
			if _, err = calllog.ScanEvents(root, collect); err != nil {
				return err
			}
			// The frames of the same call, merged in. They live in the
			// per-process files rather than the shared day stream, so they
			// are a second scan and one sort — which is the price of not
			// making a debugging switch contend with the write path that
			// decides whether a call may run.
			if _, err = calllog.ScanFramesSince(root, time.Time{}, collect); err != nil {
				return err
			}
			slices.SortStableFunc(events, func(a, b calllog.Event) int { return a.TS.Compare(b.TS) })
			if len(events) == 0 {
				return NotFoundf(CodeNotFound, "call %q not found", args[0])
			}
			out := AuditCall{CallID: args[0], Events: events}
			var warnings []string
			if payloads {
				keys := newAuditKeyCache(a, cmd)
				defer keys.close()
				if err := decryptAuditCall(root, keys, events, &out); err != nil {
					return err
				}
				warnings = append(warnings, "decrypted audit payloads may contain credentials and private user data")
			}
			return a.printer().Emit(out, warnings...)
		},
	}
	cmd.Flags().BoolVar(&payloads, "payloads", false, "decrypt and print request, effective arguments and captured result")
	return cmd
}

func decryptAuditCall(root string, keys *auditKeyCache, events []calllog.Event, out *AuditCall) error {
	for _, e := range events {
		// Frame bodies are appended in event order rather than assigned to a
		// named field: a call has exactly one request and one result, and any
		// number of frames.
		if e.Frame != nil {
			key, err := keys.get(e.Frame.KeyID)
			if err != nil {
				return err
			}
			raw, err := calllog.ReadPayload(root, *e.Frame, key)
			if err != nil {
				return err
			}
			out.Frames = append(out.Frames, string(raw))
		}
		for _, item := range []struct {
			ref *calllog.PayloadRef
			dst *string
		}{{e.Request, &out.Request}, {e.EffectiveArgs, &out.EffectiveArguments}, {e.Result, &out.Result}} {
			if item.ref == nil {
				continue
			}
			key, err := keys.get(item.ref.KeyID)
			if err != nil {
				return err
			}
			raw, err := calllog.ReadPayload(root, *item.ref, key)
			if err != nil {
				return err
			}
			*item.dst = string(raw)
		}
	}
	return nil
}

// CallsStats aggregates bounded metadata and payload byte counts.
type CallsStats struct {
	Since         time.Time      `json:"since,omitempty"`
	Events        int            `json:"events"`
	Calls         int            `json:"calls"`
	Incomplete    int            `json:"incomplete"`
	Skipped       int            `json:"skippedMalformed"`
	PayloadRaw    int64          `json:"payloadRawBytes"`
	PayloadStored int64          `json:"payloadStoredBytes"`
	Outcomes      map[string]int `json:"outcomes"`
	Clients       map[string]int `json:"clients"`
	Servers       map[string]int `json:"servers"`
	Tools         map[string]int `json:"tools"`
}

func (s CallsStats) Human(w io.Writer) error {
	_, _ = fmt.Fprintf(w, "calls: %d (%d incomplete)\nevents: %d (%d malformed skipped)\n", s.Calls, s.Incomplete, s.Events, s.Skipped)
	_, _ = fmt.Fprintf(w, "payload: %d raw bytes, %d stored bytes\n", s.PayloadRaw, s.PayloadStored)
	for _, section := range []struct {
		name string
		data map[string]int
	}{{"outcomes", s.Outcomes}, {"clients", s.Clients}, {"servers", s.Servers}, {"tools", s.Tools}} {
		if len(section.data) == 0 {
			continue
		}
		_, _ = fmt.Fprintf(w, "\n%s:\n", section.name)
		keys := make([]string, 0, len(section.data))
		for key := range section.data {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			_, _ = fmt.Fprintf(w, "  %s: %d\n", key, section.data[key])
		}
	}
	return nil
}

func (a *App) newCallsStatsCmd() *cobra.Command {
	var sinceRaw string
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Aggregate calls, outcomes and payload sizes without decryption",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			since, err := parseAuditSince(sinceRaw)
			if err != nil {
				return err
			}
			root, err := calllog.DefaultDir(a.resolver)
			if err != nil {
				return err
			}
			out := CallsStats{
				Since: since, Outcomes: map[string]int{}, Clients: map[string]int{},
				Servers: map[string]int{}, Tools: map[string]int{},
			}
			received, finished := map[string]bool{}, map[string]bool{}
			out.Skipped, err = calllog.ScanEventsSince(root, since, func(e calllog.Event) error {
				out.Events++
				if e.Kind == calllog.EventReceived {
					received[e.CallID] = true
					out.Clients[e.Client]++
				}
				if e.Kind == calllog.EventFinished {
					finished[e.CallID] = true
					out.Outcomes[e.Outcome]++
					if e.Server != "" {
						out.Servers[e.Server]++
					}
					if e.Tool != "" {
						out.Tools[e.Tool]++
					}
				}
				for _, ref := range []*calllog.PayloadRef{e.Request, e.EffectiveArgs, e.Result} {
					if ref != nil {
						out.PayloadRaw += int64(ref.RawBytes)
						out.PayloadStored += int64(ref.StoredBytes)
					}
				}
				return nil
			})
			if err != nil {
				return err
			}
			out.Calls = len(received)
			for callID := range received {
				if !finished[callID] {
					out.Incomplete++
				}
			}
			return a.printer().Emit(out)
		},
	}
	cmd.Flags().StringVar(&sinceRaw, "since", "24h", "look back by duration or RFC3339 time; use all for no time bound")
	return cmd
}

// CallsVerify is the integrity report. Independent event MACs detect edits;
// payload AEAD and bindings detect corruption or reference substitution.
type CallsVerify struct {
	OK       bool     `json:"ok"`
	Events   int      `json:"events"`
	Payloads int      `json:"payloads"`
	Skipped  int      `json:"skippedMalformed"`
	Failures int      `json:"failures"`
	Issues   []string `json:"issues,omitempty"`
}

func (v CallsVerify) Human(w io.Writer) error {
	status := "ok"
	if !v.OK {
		status = "FAILED"
	}
	_, _ = fmt.Fprintf(w, "%s: %d event(s), %d payload(s), %d failure(s), %d malformed line(s)\n",
		status, v.Events, v.Payloads, v.Failures, v.Skipped)
	for _, issue := range v.Issues {
		_, _ = fmt.Fprintf(w, "  - %s\n", issue)
	}
	return nil
}

func (a *App) newCallsVerifyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "verify",
		Short: "Authenticate metadata and decrypt every referenced payload",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			keys := newAuditKeyCache(a, cmd)
			defer keys.close()
			root, err := calllog.DefaultDir(a.resolver)
			if err != nil {
				return err
			}
			out := CallsVerify{OK: true}
			out.Skipped, err = calllog.ScanEvents(root, func(e calllog.Event) error {
				out.Events++
				key, keyErr := keys.get(e.KeyID)
				if keyErr != nil {
					out.addIssue(fmt.Sprintf("event %s/%s: %v", e.CallID, e.Kind, keyErr))
					return nil
				}
				if err := calllog.VerifyEvent(e, key); err != nil {
					out.addIssue(fmt.Sprintf("event %s/%s: %v", e.CallID, e.Kind, err))
				}
				for _, item := range []struct {
					ref  *calllog.PayloadRef
					kind calllog.PayloadKind
				}{{e.Request, calllog.PayloadRequest}, {e.EffectiveArgs, calllog.PayloadEffectiveArgs}, {e.Result, calllog.PayloadResult}} {
					if item.ref == nil {
						continue
					}
					out.Payloads++
					payloadKey, payloadKeyErr := keys.get(item.ref.KeyID)
					if payloadKeyErr != nil {
						out.addIssue(fmt.Sprintf("payload %s/%s: %v", e.CallID, item.kind, payloadKeyErr))
						continue
					}
					if err := calllog.VerifyPayload(root, *item.ref, payloadKey, e.CallID, item.kind); err != nil {
						out.addIssue(fmt.Sprintf("payload %s/%s: %v", e.CallID, item.kind, err))
					}
				}
				return nil
			})
			if err != nil {
				return err
			}
			if out.Skipped > 0 {
				out.addIssue(fmt.Sprintf("%d malformed event line(s)", out.Skipped))
			}
			if err := a.printer().Emit(out); err != nil {
				return err
			}
			if !out.OK {
				return &silentExitError{code: ExitLocked}
			}
			return nil
		},
	}
}

func (v *CallsVerify) addIssue(issue string) {
	v.OK = false
	v.Failures++
	if len(v.Issues) < 50 {
		v.Issues = append(v.Issues, issue)
	}
}

type auditKeyCache struct {
	a    *App
	cmd  *cobra.Command
	keys map[string][]byte
}

func newAuditKeyCache(a *App, cmd *cobra.Command) *auditKeyCache {
	return &auditKeyCache{a: a, cmd: cmd, keys: map[string][]byte{}}
}

func (c *auditKeyCache) get(keyID string) ([]byte, error) {
	if key := c.keys[keyID]; key != nil {
		return key, nil
	}
	key, err := c.a.loadAuditKeyID(c.cmd, keyID)
	if err != nil {
		return nil, err
	}
	c.keys[keyID] = key
	return key, nil
}

func (c *auditKeyCache) close() {
	for _, key := range c.keys {
		zeroSecret(key)
	}
}

func (a *App) loadAuditKeyID(cmd *cobra.Command, keyID string) ([]byte, error) {
	if len(keyID) != 16 {
		return nil, NotFoundf(CodeSecretNotFound, "ledger encryption key %q is invalid", keyID)
	}
	chain, _, err := a.secretChain()
	if err != nil {
		return nil, err
	}
	encoded, ok, err := chain.Get(cmd.Context(), secrets.AuditEncryptionKeyRef(keyID))
	if err == nil && !ok {
		encoded, ok, err = chain.Get(cmd.Context(), secrets.AuditEncryptionRef())
	}
	if err != nil {
		return nil, classifySecretsError(err)
	}
	if !ok {
		return nil, NotFoundf(CodeSecretNotFound, "ledger encryption key %q not found", keyID)
	}
	key, err := decodeAuditKey(encoded)
	if err != nil {
		return nil, err
	}
	got, err := calllog.KeyID(key)
	if err != nil || got != keyID {
		zeroSecret(key)
		return nil, &Error{Code: CodeStateCorrupt, ExitCode: ExitLocked, Message: fmt.Sprintf("stored audit key does not match id %q", keyID), Err: err}
	}
	return key, nil
}

func zeroSecret(key []byte) {
	for i := range key {
		key[i] = 0
	}
}
