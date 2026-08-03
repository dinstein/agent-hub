package ctlapi

import (
	"net/http"
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

	kinds := eventlog.AllKinds()
	if scope := strings.TrimSpace(q.Get("scope")); scope != "" {
		if _, ok := kinds[eventlog.Scope(scope)]; !ok {
			// A closed vocabulary exists so a caller can be TOLD it got a
			// name wrong. Answering with an empty page would be the same
			// response as "this has not happened", which is the one
			// confusion the closed set prevents.
			writeErr(w, http.StatusBadRequest, CodeBadRequest,
				"unknown scope "+strconv.Quote(scope),
				"known scopes: server, gateway, daemon", reqID)
			return
		}
		query.Scope = eventlog.Scope(scope)
	}
	for _, raw := range strings.Split(q.Get("kind"), ",") {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		k := eventlog.Kind(name)
		if !knownEventKind(kinds, query.Scope, k) {
			writeErr(w, http.StatusBadRequest, CodeBadRequest,
				"unknown kind "+strconv.Quote(name),
				"see the event-kind table in docs/modules/foundation.md", reqID)
			return
		}
		query.Kinds = append(query.Kinds, k)
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
	records := res.Records
	if len(records) > limit {
		records = records[len(records)-limit:]
	}
	out := api.EventLog{
		Events:  make([]api.EventRecord, 0, len(records)),
		Files:   len(res.Files),
		Skipped: res.Skipped,
	}
	for _, rec := range records {
		out.Events = append(out.Events, api.EventRecord{
			TS: rec.TS, Scope: string(rec.Scope), Kind: string(rec.Kind),
			Server: rec.Server, Inst: rec.Inst, Client: rec.Client, PID: rec.PID,
			From: rec.From, To: rec.To, Detail: rec.Detail,
			Attempt: rec.Attempt, DurMs: rec.DurMs,
		})
	}
	writeOK(w, http.StatusOK, out)
}

// knownEventKind validates a kind against one scope's vocabulary, or against
// all of them when no scope was named. The check is on the PAIR: a gateway
// and the daemon both `started`, and that spelling means nothing at server
// scope.
func knownEventKind(all map[eventlog.Scope][]eventlog.Kind, scope eventlog.Scope, k eventlog.Kind) bool {
	if scope != "" {
		return containsKind(all[scope], k)
	}
	for _, kinds := range all {
		if containsKind(kinds, k) {
			return true
		}
	}
	return false
}

func containsKind(kinds []eventlog.Kind, k eventlog.Kind) bool {
	for _, have := range kinds {
		if have == k {
			return true
		}
	}
	return false
}
