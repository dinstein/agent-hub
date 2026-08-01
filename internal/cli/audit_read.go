package cli

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/internal/accesslog"
	"github.com/dinstein/agent-hub/internal/secrets"
)

// AuditEventRow is the metadata-only projection used by audit tail.
type AuditEventRow struct {
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

func auditEventRow(e accesslog.Event) AuditEventRow {
	return AuditEventRow{
		Time: e.TS, CallID: e.CallID, Event: string(e.Kind), Client: e.Client,
		Face: e.Face, ExposedTool: e.Exposed, Server: e.Server, Tool: e.Tool,
		Outcome: e.Outcome, DurationMs: e.DurationMs, Code: e.Code,
		ResultCapture: e.ResultCapture,
	}
}

// AuditTail is a bounded recent-event view. Payloads are never decrypted.
type AuditTail struct {
	Since   time.Time       `json:"since,omitempty"`
	Events  []AuditEventRow `json:"events"`
	Skipped int             `json:"skippedMalformed"`
}

func (t AuditTail) Human(w io.Writer) error {
	if len(t.Events) == 0 {
		_, err := fmt.Fprintln(w, "no matching audit events")
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

func (a *App) newAuditTailCmd() *cobra.Command {
	var (
		sinceRaw string
		limit    int
		client   string
		server   string
		tool     string
		outcome  string
	)
	cmd := &cobra.Command{
		Use:   "tail",
		Short: "Show recent access metadata without decrypting payloads",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if limit <= 0 || limit > 1000 {
				return Usagef("--limit must be from 1 through 1000")
			}
			since, err := parseAuditSince(sinceRaw)
			if err != nil {
				return err
			}
			root, err := accesslog.DefaultDir(a.resolver)
			if err != nil {
				return err
			}
			rows := make([]AuditEventRow, 0, limit)
			skipped, err := accesslog.ScanEventsSince(root, since, func(e accesslog.Event) error {
				if !auditEventMatches(e, client, server, tool, outcome) {
					return nil
				}
				if len(rows) == limit {
					copy(rows, rows[1:])
					rows = rows[:limit-1]
				}
				rows = append(rows, auditEventRow(e))
				return nil
			})
			if err != nil {
				return err
			}
			return a.printer().Emit(AuditTail{Since: since, Events: rows, Skipped: skipped})
		},
	}
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

func auditEventMatches(e accesslog.Event, client, server, tool, outcome string) bool {
	return (client == "" || e.Client == client) &&
		(server == "" || e.Server == server) &&
		(tool == "" || e.Tool == tool || e.Exposed == tool) &&
		(outcome == "" || e.Outcome == outcome)
}

// AuditCall is a complete metadata lifecycle and optional decrypted payloads.
type AuditCall struct {
	CallID             string            `json:"callId"`
	Events             []accesslog.Event `json:"events"`
	Request            string            `json:"request,omitempty"`
	EffectiveArguments string            `json:"effectiveArguments,omitempty"`
	Result             string            `json:"result,omitempty"`
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
	return nil
}

func (a *App) newAuditShowCmd() *cobra.Command {
	var payloads bool
	cmd := &cobra.Command{
		Use:   "show <call-id>",
		Short: "Show one call lifecycle; decrypt payloads only with --payloads",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := accesslog.DefaultDir(a.resolver)
			if err != nil {
				return err
			}
			var events []accesslog.Event
			_, err = accesslog.ScanEvents(root, func(e accesslog.Event) error {
				if e.CallID == args[0] {
					events = append(events, e)
				}
				return nil
			})
			if err != nil {
				return err
			}
			if len(events) == 0 {
				return NotFoundf(CodeNotFound, "audit call %q not found", args[0])
			}
			out := AuditCall{CallID: args[0], Events: events}
			var warnings []string
			if payloads {
				key, err := a.loadAuditKey(cmd)
				if err != nil {
					return err
				}
				defer zeroSecret(key)
				if err := decryptAuditCall(root, key, events, &out); err != nil {
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

func decryptAuditCall(root string, key []byte, events []accesslog.Event, out *AuditCall) error {
	for _, e := range events {
		for _, item := range []struct {
			ref *accesslog.PayloadRef
			dst *string
		}{{e.Request, &out.Request}, {e.EffectiveArgs, &out.EffectiveArguments}, {e.Result, &out.Result}} {
			if item.ref == nil {
				continue
			}
			raw, err := accesslog.ReadPayload(root, *item.ref, key)
			if err != nil {
				return err
			}
			*item.dst = string(raw)
		}
	}
	return nil
}

// AuditStats aggregates bounded metadata and payload byte counts.
type AuditStats struct {
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

func (s AuditStats) Human(w io.Writer) error {
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

func (a *App) newAuditStatsCmd() *cobra.Command {
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
			root, err := accesslog.DefaultDir(a.resolver)
			if err != nil {
				return err
			}
			out := AuditStats{
				Since: since, Outcomes: map[string]int{}, Clients: map[string]int{},
				Servers: map[string]int{}, Tools: map[string]int{},
			}
			received, finished := map[string]bool{}, map[string]bool{}
			out.Skipped, err = accesslog.ScanEventsSince(root, since, func(e accesslog.Event) error {
				out.Events++
				if e.Kind == accesslog.EventReceived {
					received[e.CallID] = true
					out.Clients[e.Client]++
				}
				if e.Kind == accesslog.EventFinished {
					finished[e.CallID] = true
					out.Outcomes[e.Outcome]++
					if e.Server != "" {
						out.Servers[e.Server]++
					}
					if e.Tool != "" {
						out.Tools[e.Tool]++
					}
				}
				for _, ref := range []*accesslog.PayloadRef{e.Request, e.EffectiveArgs, e.Result} {
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

// AuditVerify is the integrity report. Independent event MACs detect edits;
// payload AEAD and bindings detect corruption or reference substitution.
type AuditVerify struct {
	OK       bool     `json:"ok"`
	Events   int      `json:"events"`
	Payloads int      `json:"payloads"`
	Skipped  int      `json:"skippedMalformed"`
	Failures int      `json:"failures"`
	Issues   []string `json:"issues,omitempty"`
}

func (v AuditVerify) Human(w io.Writer) error {
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

func (a *App) newAuditVerifyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "verify",
		Short: "Authenticate metadata and decrypt every referenced payload",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := a.loadAuditKey(cmd)
			if err != nil {
				return err
			}
			defer zeroSecret(key)
			root, err := accesslog.DefaultDir(a.resolver)
			if err != nil {
				return err
			}
			out := AuditVerify{OK: true}
			out.Skipped, err = accesslog.ScanEvents(root, func(e accesslog.Event) error {
				out.Events++
				if err := accesslog.VerifyEvent(e, key); err != nil {
					out.addIssue(fmt.Sprintf("event %s/%s: %v", e.CallID, e.Kind, err))
				}
				for _, item := range []struct {
					ref  *accesslog.PayloadRef
					kind accesslog.PayloadKind
				}{{e.Request, accesslog.PayloadRequest}, {e.EffectiveArgs, accesslog.PayloadEffectiveArgs}, {e.Result, accesslog.PayloadResult}} {
					if item.ref == nil {
						continue
					}
					out.Payloads++
					if err := accesslog.VerifyPayload(root, *item.ref, key, e.CallID, item.kind); err != nil {
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

func (v *AuditVerify) addIssue(issue string) {
	v.OK = false
	v.Failures++
	if len(v.Issues) < 50 {
		v.Issues = append(v.Issues, issue)
	}
}

func (a *App) loadAuditKey(cmd *cobra.Command) ([]byte, error) {
	chain, _, err := a.secretChain()
	if err != nil {
		return nil, err
	}
	encoded, ok, err := chain.Get(cmd.Context(), secrets.AuditEncryptionRef())
	if err != nil {
		return nil, classifySecretsError(err)
	}
	if !ok {
		return nil, NotFoundf(CodeSecretNotFound, "audit encryption key not found")
	}
	return decodeAuditKey(encoded)
}

func zeroSecret(key []byte) {
	for i := range key {
		key[i] = 0
	}
}
