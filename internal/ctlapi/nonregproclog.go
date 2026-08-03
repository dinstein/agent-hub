package ctlapi

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dinstein/agent-hub/api"
	"github.com/dinstein/agent-hub/internal/proclog"
)

// GET /v1/logs — the hub's PROCESS logs, merged across the daemon and every
// gateway, as one time-ordered page.
//
// It exists because the GUI had no view of them at all. `agenthub logs` reads
// the files directly, which a window cannot do, so the half of the record
// that matters most was terminal-only: the daemon never dials a downstream,
// so every connection failure, circuit transition, health flip and respawn is
// observed and written by a gateway process.
//
// Same shape as the other two lists — newest first, `since`/`limit`/`cursor`,
// exact-match selectors where empty means "no rule" — because a UI that shows
// the three side by side pages them the same way. The reading itself is
// internal/proclog's, shared with the CLI so the two cannot answer
// differently.

const (
	// procLogDefaultLimit is one page when the caller does not say.
	procLogDefaultLimit = 200
	// procLogMaxLimit bounds what one request may ask for. The read has to
	// buffer in order to merge, so an unbounded page is a memory decision
	// made by whoever wrote the query string.
	procLogMaxLimit = 2000
)

func (s *Server) handleProcLogs(w http.ResponseWriter, r *http.Request) {
	reqID := requestIDFrom(r.Context())
	q := r.URL.Query()

	query := proclog.Query{
		Client: strings.TrimSpace(q.Get("client")),
		Server: strings.TrimSpace(q.Get("server")),
	}
	if raw := strings.TrimSpace(q.Get("since")); raw != "" {
		ts, terr := time.Parse(time.RFC3339, raw)
		if terr != nil {
			writeErr(w, http.StatusBadRequest, CodeBadRequest,
				"since must be an RFC3339 timestamp", "choose a valid time range", reqID)
			return
		}
		query.Since = ts
	}
	if src := strings.TrimSpace(q.Get("source")); src != "" && src != "all" {
		if !slices.Contains(proclog.Origins(), src) {
			// A closed set answers a typo by SAYING so. An empty page would
			// be the same response as "nothing has been logged", which is
			// the one confusion worth spending an error on.
			writeErr(w, http.StatusBadRequest, CodeBadRequest,
				"unknown source "+strconv.Quote(src),
				"known sources: "+strings.Join(proclog.Origins(), ", "), reqID)
			return
		}
		query.Source = proclog.Origin(src)
	}
	if lvl := strings.TrimSpace(q.Get("level")); lvl != "" {
		if err := query.MinLevel.UnmarshalText([]byte(strings.ToUpper(lvl))); err != nil {
			writeErr(w, http.StatusBadRequest, CodeBadRequest,
				"unknown level "+strconv.Quote(lvl),
				"known levels: debug, info, warn, error", reqID)
			return
		}
		query.Leveled = true
	}

	var cursor pageCursor
	if raw := strings.TrimSpace(q.Get("cursor")); raw != "" {
		var err error
		cursor, err = decodePageCursor(raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, CodeBadRequest,
				"cursor is invalid", "return to the first page", reqID)
			return
		}
	}

	limit := procLogDefaultLimit
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > procLogMaxLimit {
			writeErr(w, http.StatusBadRequest, CodeBadRequest,
				"limit must be from 1 through "+strconv.Itoa(procLogMaxLimit),
				"choose a smaller page size", reqID)
			return
		}
		limit = n
	}

	records, err := proclog.Read(s.opts.NonRegistry.LogsDir, query)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeInternal,
			"reading the process logs failed: "+err.Error(), "", reqID)
		return
	}
	// proclog returns oldest first, which is right for a tail and wrong for a
	// pager.
	slices.Reverse(records)
	if !cursor.isZero() {
		start := 0
		for start < len(records) && !cursor.after(records[start].TS, procLogTie(records[start])) {
			start++
		}
		records = records[start:]
	}

	out := api.ProcLogPage{Total: len(records)}
	if len(records) > limit {
		records = records[:limit]
		if n := len(records); n > 0 {
			out.NextCursor = encodePageCursor(records[n-1].TS, procLogTie(records[n-1]))
		}
	}
	out.Records = make([]api.ProcLogRecord, 0, len(records))
	for _, rec := range records {
		out.Records = append(out.Records, api.ProcLogRecord{
			Time: rec.TS, Origin: string(rec.Origin), Level: rec.Text, Message: rec.Msg,
			Client: rec.Field("client"), Server: rec.Field("server"),
			PID: intField(rec.Fields, "pid"), Fields: extraFields(rec.Fields),
		})
	}
	writeOK(w, http.StatusOK, out)
}

// procLogTie breaks records sharing a timestamp: the process that wrote it
// and the message it wrote. Two records agreeing on both in the same
// nanosecond are the same line reported twice.
func procLogTie(r proclog.Record) string {
	return string(r.Origin) + "/" + strconv.Itoa(intField(r.Fields, "pid")) + "/" + r.Msg
}

func intField(fields map[string]any, key string) int {
	if n, ok := fields[key].(float64); ok {
		return int(n)
	}
	return 0
}

// extraFields is everything the record carries beyond the columns above,
// rendered as strings so one row is one flat object a table can show.
//
// The three that became columns are dropped, and so are the three slog owns
// (time, level, msg): repeating them would put the same fact in a row twice,
// and a UI reading the second copy is exactly the duplicate-key problem the
// log convention exists to prevent.
func extraFields(fields map[string]any) map[string]string {
	var out map[string]string
	for k, v := range fields {
		switch k {
		case "time", "level", "msg", "client", "server", "pid":
			continue
		}
		if out == nil {
			out = make(map[string]string, len(fields))
		}
		out[k] = fmt.Sprintf("%v", v)
	}
	return out
}
