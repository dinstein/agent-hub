package api

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The control-plane EVENT LOG, which is not the SSE stream in events.go
// despite both living on EventsService.
//
// The stream is a change NOTIFICATION channel: a subscriber that misses a
// frame re-reads state, and the payload is a snapshot. The log is the record
// of what actually moved — connected, circuit_open, respawn_failed — in a
// CLOSED vocabulary a consumer may switch on. They answer "something changed
// just now" and "what happened to this server", and a UI needs both.

// EventRecord is one control-plane state change.
//
// The wire shape mirrors internal/eventlog.Record, which api may not import
// (depguard rule 1). Kind and Scope are closed sets published in
// docs/modules/foundation.md; a consumer may match on them, and an unknown
// value means this client is older than the daemon rather than that the
// value is free text.
type EventRecord struct {
	TS    time.Time `json:"ts"`
	Scope string    `json:"scope"`
	Kind  string    `json:"kind"`
	// Server and Inst identify a downstream and, for a derived instance, the
	// derivation. Server is set at scope "server" only.
	Server string `json:"server,omitempty"`
	Inst   string `json:"inst,omitempty"`
	// Client is the client whose gateway observed this. Empty for daemon
	// records, which belong to no client.
	Client string `json:"client,omitempty"`
	// PID is which process wrote the record. Never omitted: several
	// processes append to one file, and a record attributable to none of
	// them cannot be placed in a timeline.
	PID int `json:"pid"`
	// From and To carry a state transition where the kind has one.
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	// Detail elaborates a failure.
	Detail string `json:"detail,omitempty"`
	// Count is the one number the kind carries, and the KIND decides what it
	// counts: tools for `connected` and `tools_changed`, respawns for
	// `respawned`, consecutive failures for `circuit_open` and the health
	// flips. The mapping is published beside the kinds in
	// docs/modules/foundation.md, and a consumer that renders the number
	// without consulting it labels a thirteen-tool connect a thirteenth try.
	Count int `json:"count,omitempty"`
	// Rev is a registry generation. It IDENTIFIES a config revision rather
	// than counting anything, which is why it is not folded into Count.
	Rev   uint64 `json:"rev,omitempty"`
	DurMs int64  `json:"durMs,omitempty"`
}

// EventLog is the GET /v1/events/log response.
type EventLog struct {
	Events []EventRecord `json:"events"`
	// Files is how many segments the read covered. Zero means nothing has
	// ever been written, which is a different fact from an empty Events over
	// four files — only the first can mean the stream is switched off.
	Files int `json:"files"`
	// Skipped counts undecodable lines.
	Skipped int `json:"skipped,omitempty"`
}

// EventLogFilter narrows a read. The zero value reads the recent tail of
// everything.
type EventLogFilter struct {
	Since  time.Time
	Scope  string
	Server string
	Client string
	Kinds  []string
	// Limit bounds the returned tail (server-side default when 0).
	Limit int
}

func eventLogQuery(f EventLogFilter) url.Values {
	q := url.Values{}
	if !f.Since.IsZero() {
		q.Set("since", f.Since.UTC().Format(time.RFC3339))
	}
	if f.Scope != "" {
		q.Set("scope", f.Scope)
	}
	if f.Server != "" {
		q.Set("server", f.Server)
	}
	if f.Client != "" {
		q.Set("client", f.Client)
	}
	if len(f.Kinds) > 0 {
		q.Set("kind", strings.Join(f.Kinds, ","))
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	return q
}

// Log reads the control-plane event log, newest last.
func (s *EventsService) Log(ctx context.Context, f EventLogFilter) (EventLog, error) {
	var out EventLog
	err := s.c.do(ctx, http.MethodGet, "/events/log", eventLogQuery(f), nil, &out)
	return out, err
}
