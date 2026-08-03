package api

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// The process logs: what the daemon and the gateways DID, as opposed to what
// a client called (calls.go) or what state changed (eventlog.go).
//
// It is the third of the three observability reads and the last to be served
// over this link. The files are readable from a terminal, which is why the
// CLI never needed it; a window cannot read a file, so without this the GUI
// could not show the half of the record that explains a downstream failure —
// the daemon never dials a downstream, and the gateway that does writes to
// its own file.

// ProcLogRecord is one line of a process log — daemon.log or a
// gateway-<client>.log — as the control plane serves it.
//
// The three join keys are columns because every consumer wants them and a
// map lookup per row to find the client is a worse API than a field. The
// rest stay in Fields, flattened to strings: slog attrs are open-ended by
// design, and a UI that showed only the ones this struct happened to name
// would silently drop whatever a new log line adds.
type ProcLogRecord struct {
	Time    time.Time `json:"time"`
	Origin  string    `json:"origin"`
	Level   string    `json:"level"`
	Message string    `json:"msg"`
	Client  string    `json:"client,omitempty"`
	Server  string    `json:"server,omitempty"`
	PID     int       `json:"pid,omitempty"`
	// Fields is every remaining slog attribute, as text.
	Fields map[string]string `json:"fields,omitempty"`
}

// ProcLogPage is the GET /v1/logs response: newest first, with the same
// cursor contract the calls and events lists use.
type ProcLogPage struct {
	Records []ProcLogRecord `json:"records"`
	// Total is how many records matched from the cursor onward.
	Total int `json:"total,omitempty"`
	// NextCursor is the position to resume from, empty at the end.
	NextCursor string `json:"nextCursor,omitempty"`
}

// ProcLogFilter narrows a process-log read. Empty means "no rule".
type ProcLogFilter struct {
	Since time.Time
	// Source is "daemon" or "gateway"; empty reads both.
	Source string
	// Level is the minimum: debug, info, warn or error.
	Level          string
	Client, Server string
	Limit          int
	Cursor         string
}

// LogsService reads the hub's process logs — what the daemon and the gateways
// were doing — as opposed to CallsService (what a client called) and the
// event log (what state changed).
type LogsService struct{ c *Client }

// List returns one page, newest first.
func (s *LogsService) List(ctx context.Context, f ProcLogFilter) (ProcLogPage, error) {
	var out ProcLogPage
	err := s.c.do(ctx, http.MethodGet, "/logs", procLogQuery(f), nil, &out)
	return out, err
}

func procLogQuery(f ProcLogFilter) url.Values {
	q := url.Values{}
	if !f.Since.IsZero() {
		q.Set("since", f.Since.UTC().Format(time.RFC3339))
	}
	for key, value := range map[string]string{
		"source": f.Source, "level": f.Level, "client": f.Client,
		"server": f.Server, "cursor": f.Cursor,
	} {
		if value != "" {
			q.Set(key, value)
		}
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	return q
}
