package cli

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/internal/eventlog"
)

// `events` reads the control-plane event stream: what happened to a
// downstream server, a gateway or the daemon, in the closed vocabulary of
// internal/eventlog.
//
// WHAT THIS COMMAND USED TO BE. It subscribed to the daemon's SSE stream,
// was online-only (exit 4 without a daemon), and rendered whatever that
// stream carried — which, per the contract in internal/event, is
// deliberately a change NOTIFICATION channel and NOT a change log: a
// subscriber that misses a frame re-reads state, and the payload is a full
// snapshot rather than a description of what moved. So the old command
// answered "something about servers changed just now" and could not answer
// "what happened to this server", which is the question people brought to
// it.
//
// The event stream IS the change log, so the verb moves to it. Two
// consequences follow, and both are improvements:
//
//   - It works OFFLINE, and must. A stdio gateway writes this file with no
//     daemon anywhere in the picture, so an installation without one is not
//     a degraded case here — it is the ordinary one, and the old exit 4
//     refused exactly the setup with the most to explain.
//   - --follow tails the file rather than holding a subscription, so it
//     survives a daemon restart instead of ending on one.
const (
	// eventsDefaultLimit is how many records a bare `events` shows. The
	// stream is low-rate, but an incident-scale window still wants a tail
	// rather than the whole file.
	eventsDefaultLimit = 100
	// eventsFollowInterval is the --follow re-read period. It matches
	// serverLogsInterval; the two process-log readers share a shorter
	// logsFollowInterval, and `session ls -f` a longer one because it polls
	// the daemon rather than a file.
	eventsFollowInterval = 500 * time.Millisecond
	// eventsDetailWidth caps the DETAIL column. The full text is in --json.
	eventsDetailWidth = 90
)

// EventRow is one record as both output modes render it.
type EventRow struct {
	TS    string `json:"ts"`
	Scope string `json:"scope"`
	Kind  string `json:"kind"`
	// Class is routine or disruption. It is a projection of Kind rather than
	// a field of the record: deriving it here means an old file gains the
	// answer as soon as the binary knows it, and no writer can disagree with
	// a reader about what its own kind means.
	Class  string `json:"class"`
	Server string `json:"server,omitempty"`
	Inst   string `json:"inst,omitempty"`
	Client string `json:"client,omitempty"`
	// Session is set by the kinds that are about one MCP session on the HTTP
	// face, which belong to no configured client.
	Session string `json:"session,omitempty"`
	PID     int    `json:"pid,omitempty"`
	From    string `json:"from,omitempty"`
	To      string `json:"to,omitempty"`
	Detail  string `json:"detail,omitempty"`
	// Count is the one number the kind carries; eventlog.CountNoun says what
	// it counts. The record has one such field and so does its projection.
	Count int `json:"count,omitempty"`
	// Rev is a config generation, not a count — see eventlog.Record.
	Rev   uint64 `json:"rev,omitempty"`
	DurMs int64  `json:"durMs,omitempty"`
}

// EventList is the `events` result.
type EventList struct {
	Events []EventRow `json:"events"`
	// Files names every segment the read covered. "No events" over four
	// files is a different fact from "no events" over none, and only the
	// second one means nothing has ever been recorded.
	Files []string `json:"files,omitempty"`
	// Skipped counts undecodable lines.
	Skipped int `json:"skipped,omitempty"`
	// Note carries the "nothing here yet" diagnosis, so a --json consumer
	// sees the same explanation a human does.
	Note string `json:"note,omitempty"`
}

// Human renders the table.
func (l EventList) Human(w io.Writer) error {
	if len(l.Events) == 0 {
		if l.Note != "" {
			_, err := fmt.Fprintln(w, l.Note)
			return err
		}
		_, err := fmt.Fprintln(w, "no events recorded yet")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "TIME\tSCOPE\tKIND\tSUBJECT\tDETAIL")
	for _, e := range l.Events {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			e.TS, e.Scope, e.Kind, dash(eventSubject(e)),
			oneLine(eventDetail(e), eventsDetailWidth))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if l.Skipped > 0 {
		_, err := fmt.Fprintf(w, "%d undecodable line(s) skipped\n", l.Skipped)
		return err
	}
	return nil
}

// eventSubject is what the record is ABOUT: a server (with its derived
// instance when there is one), the client whose gateway spoke, or — on the
// HTTP face, whose callers are tokens rather than configured clients — the
// session itself.
//
// The fallbacks are ordered narrowest-first. A login carries both a server
// and a session, and the server is what somebody scanning the column is
// looking for; a bound HTTP session has only the session.
func eventSubject(e EventRow) string {
	switch {
	case e.Server != "" && e.Inst != "":
		return e.Server + "/" + e.Inst
	case e.Server != "":
		return e.Server
	case e.Client != "":
		return e.Client
	default:
		return e.Session
	}
}

// eventDetail folds the transition, the count and the free text into one
// column, in that order — a flip is the fact, and the prose elaborates it.
func eventDetail(e EventRow) string {
	var parts []string
	if e.From != "" || e.To != "" {
		parts = append(parts, dash(e.From)+"->"+dash(e.To))
	}
	if e.Count != 0 {
		parts = append(parts, countPhrase(e.Kind, e.Count))
	}
	if e.Rev != 0 {
		parts = append(parts, fmt.Sprintf("rev %d", e.Rev))
	}
	if e.DurMs != 0 {
		parts = append(parts, fmt.Sprintf("%dms", e.DurMs))
	}
	if e.Detail != "" {
		parts = append(parts, e.Detail)
	}
	return strings.Join(parts, " ")
}

// countPhrase renders Count with the noun its kind gives it.
//
// The fallback is a bare `n=13`, and it is what a kind this build does not
// know gets — a record from a newer daemon still prints its number rather
// than losing it. That is also what every count read as before the noun
// existed, which is why a connect that listed thirteen tools looked like a
// thirteenth attempt.
func countPhrase(kind string, n int) string {
	if noun := eventlog.CountNoun(eventlog.Kind(kind)); noun != "" {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("n=%d", n)
}

func (a *App) newEventsCmd() *cobra.Command {
	var (
		follow bool
		since  time.Duration
		limit  int
		scope  string
		server string
		client string
		class  string
		kinds  []string
	)
	cmd := &cobra.Command{
		Use:   "events [--server <id>] [--since 24h] [-f]",
		Short: "Read server, gateway and daemon state changes",
		Long: "Reads <data>/logs/events.jsonl — every state change of a downstream\n" +
			"server, a gateway or the daemon, in a closed vocabulary.\n\n" +
			"Scopes: " + strings.Join(eventlog.ScopeNames(), ", ") + ".\n" +
			"--class " + strings.Join(eventlog.ClassNames(), "/") + " splits the hub running as\n" +
			"intended from the hub reacting to something that went wrong. A disruption\n" +
			"keeps the recovery that ended it, so an outage never reads as still open.\n\n" +
			"Works offline: a stdio gateway writes this file with no daemon running.\n\n" +
			"See also `agenthub logs` for the same processes' prose, and\n" +
			"`agenthub calls` for what a client CALLED.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			q, err := eventQuery(scope, server, client, class, kinds, since)
			if err != nil {
				return err
			}
			logsDir, err := a.resolver.LogsDir()
			if err != nil {
				return err
			}
			path := filepath.Join(logsDir, eventlog.FileName)
			if follow {
				return a.followEvents(cmd.Context(), path, q, limit)
			}
			list, err := readEventList(path, q, limit)
			if err != nil {
				return err
			}
			return a.printer().Emit(list)
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "stay open and keep printing new events")
	cmd.Flags().DurationVar(&since, "since", 0, "only events newer than this age (e.g. 24h)")
	cmd.Flags().IntVar(&limit, "limit", eventsDefaultLimit, "how many events to show (0 = all of them)")
	cmd.Flags().StringVar(&scope, "scope", "", "only this scope: "+strings.Join(eventlog.ScopeNames(), ", "))
	cmd.Flags().StringVar(&server, "server", "", "only events about this downstream server")
	cmd.Flags().StringVar(&client, "client", "", "only events observed by this client's gateway")
	cmd.Flags().StringVar(&class, "class", "",
		"only this class: "+strings.Join(eventlog.ClassNames(), ", "))
	cmd.Flags().StringSliceVar(&kinds, "kind", nil, "only these kinds (comma separated)")
	return cmd
}

// eventQuery validates the selectors and builds the read query.
//
// An unknown scope or kind is a USAGE ERROR, not an empty result. The
// vocabulary is closed precisely so a caller can be told it got a name
// wrong; answering a typo with "no events" hands back the same output as
// "this has not happened", which is the one confusion a closed set exists to
// prevent.
//
// Both the check and the hint come from internal/eventlog. This command and
// the control plane's /events/log ask the same question of the same closed
// set, and a local copy of the answer is one that can be right while the
// other is wrong — which is how the control plane ended up hinting a list of
// scopes written by hand.
func eventQuery(scope, server, client, class string, kinds []string, since time.Duration) (eventlog.Query, error) {
	q := eventlog.Query{Server: server, Client: client}
	if since > 0 {
		q.Since = time.Now().Add(-since)
	}
	if scope != "" {
		if !eventlog.KnownScope(eventlog.Scope(scope)) {
			e := Usagef("unknown scope %q", scope)
			e.Hint = "known scopes: " + strings.Join(eventlog.ScopeNames(), ", ")
			return eventlog.Query{}, e
		}
		q.Scope = eventlog.Scope(scope)
	}
	if class != "" {
		if !eventlog.KnownClass(eventlog.Class(class)) {
			e := Usagef("unknown class %q", class)
			e.Hint = "known classes: " + strings.Join(eventlog.ClassNames(), ", ")
			return eventlog.Query{}, e
		}
		q.Class = eventlog.Class(class)
	}
	for _, raw := range kinds {
		k := eventlog.Kind(strings.TrimSpace(raw))
		if !eventlog.KnownKind(q.Scope, k) {
			e := Usagef("unknown kind %q", raw)
			e.Hint = "known kinds: " + strings.Join(eventlog.KindNames(q.Scope), ", ")
			return eventlog.Query{}, e
		}
		q.Kinds = append(q.Kinds, k)
	}
	return q, nil
}

// readEventList reads and projects, keeping the newest `limit` records.
func readEventList(path string, q eventlog.Query, limit int) (EventList, error) {
	res, err := eventlog.Read(path, q)
	if err != nil {
		return EventList{}, err
	}
	return listOf(tailOf(res.Records, limit), res, path), nil
}

// tailOf keeps the newest limit records; 0 means all of them.
func tailOf(records []eventlog.Record, limit int) []eventlog.Record {
	if limit > 0 && len(records) > limit {
		return records[len(records)-limit:]
	}
	return records
}

// listOf projects records into the result, carrying the read's provenance.
func listOf(records []eventlog.Record, res eventlog.Result, path string) EventList {
	list := EventList{Events: rowsOfEvents(records), Files: res.Files, Skipped: res.Skipped}
	if len(res.Files) == 0 {
		list.Note = fmt.Sprintf("no event stream at %s yet; any running gateway or daemon "+
			"writes it unless 'events.enabled' is false", path)
	}
	return list
}

func rowsOfEvents(records []eventlog.Record) []EventRow {
	out := make([]EventRow, 0, len(records))
	for _, r := range records {
		out = append(out, EventRow{
			TS: r.TS.UTC().Format(time.RFC3339), Scope: string(r.Scope), Kind: string(r.Kind),
			Class:  string(eventlog.ClassOf(r.Kind)),
			Server: r.Server, Inst: r.Inst, Client: r.Client, Session: r.Session, PID: r.PID,
			From: r.From, To: r.To, Detail: r.Detail,
			Count: r.Count, Rev: r.Rev, DurMs: r.DurMs,
		})
	}
	return out
}

// followEvents prints the current tail, then every newly appended record.
//
// It tracks a record TIMESTAMP rather than a byte offset, unlike the other
// two followers. This stream rotates into segments that Read walks as one
// sequence, so a byte offset into "the file" is not a position in the stream
// at all: after a rotation it points into a segment that is no longer the
// end.
//
// The cursor is the RECORD's time.Time, not the rendered stamp. Reading it
// back out of the projected row meant parsing an RFC3339 string with second
// resolution, and the cursor then advanced a whole second past records that
// had not been printed — so a burst inside one second silently lost its
// tail. Records carry nanoseconds; keeping the cursor on the record side
// costs nothing and the loss disappears.
func (a *App) followEvents(ctx context.Context, path string, q eventlog.Query, limit int) error {
	p := a.printer()
	res, err := eventlog.Read(path, q)
	if err != nil {
		return err
	}
	records := tailOf(res.Records, limit)
	if err := p.Emit(listOf(records, res, path)); err != nil {
		return err
	}
	seen := newest(records)
	ticker := time.NewTicker(eventsFollowInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		res, err := eventlog.Read(path, q)
		if err != nil {
			return err
		}
		fresh := res.Records
		if !seen.IsZero() {
			fresh = after(fresh, seen)
		}
		if len(fresh) == 0 {
			continue
		}
		seen = newest(fresh)
		if err := p.Emit(EventList{Events: rowsOfEvents(fresh)}); err != nil {
			return err
		}
	}
}

// newest is the timestamp of the last record, or the zero time.
func newest(records []eventlog.Record) time.Time {
	if len(records) == 0 {
		return time.Time{}
	}
	return records[len(records)-1].TS
}

// after keeps records strictly newer than ts.
//
// Strictly, so a record sharing an instant with the last printed one is
// dropped rather than printed twice — and at nanosecond resolution that is a
// genuine tie rather than the whole second the rendered stamp used to round
// to. A duplicate is the worse failure in a stream someone is watching for a
// state change: it reads as the state having changed twice.
//
// Read returns records sorted by time, so the first one past the cursor
// begins the tail.
func after(records []eventlog.Record, ts time.Time) []eventlog.Record {
	for i, r := range records {
		if r.TS.After(ts) {
			return records[i:]
		}
	}
	return nil
}
