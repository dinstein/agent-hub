package ctlapi

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dinstein/agent-hub/api"
	"github.com/dinstein/agent-hub/internal/eventlog"
)

// GET /v1/events/log — the control-plane event log, for the GUI.
//
// This route exists because cmd/agenthub-gui MAY NOT import internal/*
// (AGENTS.md hard constraint 1, proven by internal/depguardtest). The CLI
// reads internal/eventlog directly and works offline; the GUI reads it here
// and therefore needs a daemon, which is the same trade every other page in
// that application already makes.
//
// It is a READ of a file this process does not own exclusively — N gateways
// append to it — so it opens nothing for writing and holds no lock. That is
// the same discipline `agenthub events` follows, and it is why running both
// at once cannot disturb a writer.

const (
	// eventLogDefaultLimit is the tail size when a caller names none.
	eventLogDefaultLimit = 200
	// eventLogMaxLimit bounds one page. A UI that wants more paginates by
	// `since`, which the stream is already ordered by.
	eventLogMaxLimit = 2000
)

// eventTie breaks records sharing a timestamp. Scope, kind, subject and pid
// is what a record has that is stable across reads; two records agreeing on
// all four in the same nanosecond are the same state change reported twice.
func eventTie(r eventlog.Record) string {
	return string(r.Scope) + "/" + string(r.Kind) + "/" + r.Server + r.Client + "/" + strconv.Itoa(r.PID)
}

func (s *Server) handleEventLog(w http.ResponseWriter, r *http.Request) {
	reqID := requestIDFrom(r.Context())
	q := r.URL.Query()

	var query eventlog.Query
	if raw := strings.TrimSpace(q.Get("since")); raw != "" {
		ts, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, CodeBadRequest,
				"since must be an RFC3339 timestamp", "choose a valid time range", reqID)
			return
		}
		query.Since = ts
	}
	query.Server = strings.TrimSpace(q.Get("server"))
	query.Client = strings.TrimSpace(q.Get("client"))

	// The vocabulary answers for itself, from the same source `agenthub
	// events` reads. The alternative is what stood here: a list of scopes
	// written out by hand, which keeps reading convincingly after the real
	// one has moved.
	if scope := strings.TrimSpace(q.Get("scope")); scope != "" {
		if !eventlog.KnownScope(eventlog.Scope(scope)) {
			// A closed vocabulary exists so a caller can be TOLD it got a
			// name wrong. Answering with an empty page would be the same
			// response as "this has not happened", which is the one
			// confusion the closed set prevents.
			writeErr(w, http.StatusBadRequest, CodeBadRequest,
				"unknown scope "+strconv.Quote(scope),
				"known scopes: "+strings.Join(eventlog.ScopeNames(), ", "), reqID)
			return
		}
		query.Scope = eventlog.Scope(scope)
	}
	if class := strings.TrimSpace(q.Get("class")); class != "" {
		if !eventlog.KnownClass(eventlog.Class(class)) {
			writeErr(w, http.StatusBadRequest, CodeBadRequest,
				"unknown class "+strconv.Quote(class),
				"known classes: "+strings.Join(eventlog.ClassNames(), ", "), reqID)
			return
		}
		query.Class = eventlog.Class(class)
	}
	for _, raw := range strings.Split(q.Get("kind"), ",") {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		k := eventlog.Kind(name)
		if !eventlog.KnownKind(query.Scope, k) {
			writeErr(w, http.StatusBadRequest, CodeBadRequest,
				"unknown kind "+strconv.Quote(name),
				"known kinds: "+strings.Join(eventlog.KindNames(query.Scope), ", "), reqID)
			return
		}
		query.Kinds = append(query.Kinds, k)
	}

	var cursor pageCursor
	if raw := strings.TrimSpace(q.Get("cursor")); raw != "" {
		var cerr error
		cursor, cerr = decodePageCursor(raw)
		if cerr != nil {
			writeErr(w, http.StatusBadRequest, CodeBadRequest,
				"cursor is invalid", "return to the first page", reqID)
			return
		}
	}

	limit := eventLogDefaultLimit
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > eventLogMaxLimit {
			writeErr(w, http.StatusBadRequest, CodeBadRequest,
				"limit must be from 1 through "+strconv.Itoa(eventLogMaxLimit),
				"choose a smaller page size", reqID)
			return
		}
		limit = n
	}

	res, err := eventlog.Read(s.opts.NonRegistry.EventLogPath, query)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeInternal,
			"reading the event log failed: "+err.Error(), "", reqID)
		return
	}
	// Newest first, so a page is a prefix and a cursor is the last row of
	// one — the same model the calls list uses. The reader hands them back
	// oldest first, which is the right order for a tail and the wrong one
	// for a pager.
	records := res.Records
	slices.Reverse(records)
	if !cursor.isZero() {
		start := 0
		for start < len(records) && !cursor.after(records[start].TS, eventTie(records[start])) {
			start++
		}
		records = records[start:]
	}
	out := api.EventLog{
		Files:   len(res.Files),
		Skipped: res.Skipped,
		Total:   len(records),
	}
	if len(records) > limit {
		records = records[:limit]
		if n := len(records); n > 0 {
			out.NextCursor = encodePageCursor(records[n-1].TS, eventTie(records[n-1]))
		}
	}
	out.Events = make([]api.EventRecord, 0, len(records))
	for _, rec := range records {
		out.Events = append(out.Events, api.EventRecord{
			TS: rec.TS, Scope: string(rec.Scope), Kind: string(rec.Kind),
			Class:  string(eventlog.ClassOf(rec.Kind)),
			Server: rec.Server, Inst: rec.Inst, Client: rec.Client,
			Session: rec.Session, PID: rec.PID,
			From: rec.From, To: rec.To, Detail: rec.Detail,
			Count: rec.Count, Rev: rec.Rev, DurMs: rec.DurMs,
		})
	}
	writeOK(w, http.StatusOK, out)
}
