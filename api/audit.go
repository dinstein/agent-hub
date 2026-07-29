package api

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Audit stream names (four independent streams). They are
// display/identification labels — the client addresses a stream by ROUTE
// (Tail vs TailSecurity), never by passing one of these as a selector.
const (
	// AuditStreamAudit is the call ledger: one record per executed call.
	AuditStreamAudit = "audit"
	// AuditStreamSecurity is the guard/governance event stream.
	AuditStreamSecurity = "security"
)

// maxAuditTail bounds a tail request client-side. The daemon clamps too;
// bounding here keeps a mis-typed limit from asking for a huge body.
const maxAuditTail = 1000

// AuditRecord is one entry of the call ledger.
//
// Args red line: a record carries ArgsHash only — argument bytes never enter
// the audit stream (docs/modules/foundation.md). A frontend that wants to show arguments
// must use the live inspect ring buffer, which is a separate, opt-in surface.
//
// CONTRACT: field names mirror internal/audit.Record (camelCase JSON tags,
// unlike the snake_case control-plane DTOs — the ledger shape is the file
// format and is not re-cased on the way out).
type AuditRecord struct {
	TS      time.Time `json:"ts"`
	Actor   string    `json:"actor"`
	Client  string    `json:"client"`
	Session string    `json:"session"`
	Server  string    `json:"server"`
	Tool    string    `json:"tool"`
	// ArgsHash binds the record to the arguments without carrying them.
	ArgsHash string `json:"argsHash"`
	// Decision is the terminal outcome ("allowed", "denied", ...).
	Decision  string `json:"decision"`
	DurMs     int64  `json:"durMs"`
	RequestID string `json:"requestID"`
}

// SecurityEvent is one entry of the security stream: a guard verdict, a
// drift detection or another governance decision.
//
// CONTRACT: mirrors internal/audit.SecurityEvent.
type SecurityEvent struct {
	TS    time.Time `json:"ts"`
	Event string    `json:"event"`
	// Severity is the event severity ("info", "warn", "critical").
	Severity  string `json:"severity"`
	Server    string `json:"server,omitempty"`
	Tool      string `json:"tool,omitempty"`
	Client    string `json:"client,omitempty"`
	Detail    string `json:"detail,omitempty"`
	RequestID string `json:"requestID,omitempty"`
}

// AuditService reads the tail of the audit streams.
//
// The two streams are two ROUTES (GET /v1/audit and GET /v1/security), not
// one route with a stream selector: they carry different record types, and a
// mis-spelled selector that silently returned the wrong stream would be a
// governance surface reading the wrong ledger.
//
// Tail is the backfill, not the feed: live records arrive on the `activity`
// SSE topic, which is the primary source for an activity view. A GUI must
// use these routes rather than reading the ledger files — the data directory
// is off-limits to it (docs/modules/controlplane.md).
type AuditService struct{ c *Client }

// Tail returns up to limit most recent records of the call ledger, oldest
// first. limit <= 0 selects the daemon default; it is clamped to 1000.
func (s *AuditService) Tail(ctx context.Context, limit int) ([]AuditRecord, error) {
	var out []AuditRecord
	if err := s.c.do(ctx, http.MethodGet, "/audit", tailQuery(limit), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// TailSecurity returns up to limit most recent security events, oldest
// first. limit <= 0 selects the daemon default; it is clamped to 1000.
func (s *AuditService) TailSecurity(ctx context.Context, limit int) ([]SecurityEvent, error) {
	var out []SecurityEvent
	if err := s.c.do(ctx, http.MethodGet, "/security", tailQuery(limit), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// tailQuery bounds the request client-side too: the daemon clamps, but
// bounding here keeps a mis-typed limit from asking for a huge body.
func tailQuery(limit int) url.Values {
	if limit <= 0 {
		return nil
	}
	return url.Values{"limit": {strconv.Itoa(min(limit, maxAuditTail))}}
}
