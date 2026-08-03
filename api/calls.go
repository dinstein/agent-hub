package api

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// CallsStatus is the effective access-ledger policy and its current storage
// footprint. KeyID is public key metadata; encryption key material never
// crosses the control plane.
type CallsStatus struct {
	Generation    uint64     `json:"generation"`
	Enabled       bool       `json:"enabled"`
	Arguments     string     `json:"arguments"`
	Results       string     `json:"results"`
	ResultBytes   int        `json:"resultBytes"`
	Durability    string     `json:"durability"`
	RetentionDays int        `json:"retentionDays"`
	MaxBytes      int64      `json:"maxBytes"`
	MinFreeBytes  int64      `json:"minFreeBytes"`
	Pressure      string     `json:"pressure"`
	KeyID         string     `json:"keyId,omitempty"`
	Storage       AuditUsage `json:"storage"`
}

type AuditUsage struct {
	Bytes      int64 `json:"bytes"`
	Days       int   `json:"days"`
	EventFiles int   `json:"eventFiles"`
	PackFiles  int   `json:"packFiles"`
}

// CallSummary joins one call's immutable lifecycle into a list-safe,
// metadata-only row. Payload references are deliberately not exposed.
type CallSummary struct {
	CallID        string    `json:"callId"`
	Time          time.Time `json:"time"`
	Client        string    `json:"client,omitempty"`
	Face          string    `json:"face,omitempty"`
	ExposedTool   string    `json:"exposedTool,omitempty"`
	Server        string    `json:"server,omitempty"`
	Tool          string    `json:"tool,omitempty"`
	Outcome       string    `json:"outcome,omitempty"`
	DurationMs    int64     `json:"durationMs,omitempty"`
	Code          string    `json:"code,omitempty"`
	ResultCapture string    `json:"resultCapture,omitempty"`
	Complete      bool      `json:"complete"`
}

type CallPage struct {
	Since      time.Time     `json:"since,omitempty"`
	Calls      []CallSummary `json:"calls"`
	Total      int           `json:"total"`
	NextCursor string        `json:"nextCursor,omitempty"`
	Skipped    int           `json:"skippedMalformed"`
}

type AuditEvent struct {
	Time       time.Time `json:"time"`
	Event      string    `json:"event"`
	RequestID  string    `json:"requestId,omitempty"`
	Session    string    `json:"session,omitempty"`
	PolicyRev  uint64    `json:"policyRev,omitempty"`
	Server     string    `json:"server,omitempty"`
	Tool       string    `json:"tool,omitempty"`
	Outcome    string    `json:"outcome,omitempty"`
	DurationMs int64     `json:"durationMs,omitempty"`
	Gate       string    `json:"gate,omitempty"`
	Rule       string    `json:"rule,omitempty"`
	Code       string    `json:"code,omitempty"`
	Error      string    `json:"error,omitempty"`
	ToolError  bool      `json:"toolError,omitempty"`
	// Method, Cause, Seq and Bytes describe a FRAME (event "sent" or "recv"):
	// what crossed the downstream boundary, why, which attempt it was, and
	// how big it was. Empty on the three lifecycle events.
	Method string `json:"method,omitempty"`
	Cause  string `json:"cause,omitempty"`
	Seq    int    `json:"seq,omitempty"`
	Bytes  int    `json:"bytes,omitempty"`
}

// AuditPayload is decrypted only for one explicitly selected call. Truncated
// means the GUI preview is shorter than the authenticated ledger payload.
type AuditPayload struct {
	Text      string `json:"text,omitempty"`
	Bytes     int    `json:"bytes,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

type CallDetail struct {
	CallSummary
	// Events is the whole story in order: the lifecycle at the client
	// boundary and, for a traced server, the frames at the downstream one.
	// One `routed` can be followed by several sent/recv pairs — that is a
	// retry, and it is the thing neither stream could show before the frames
	// moved into the ledger.
	Events             []AuditEvent `json:"events"`
	Error              string       `json:"error,omitempty"`
	Request            AuditPayload `json:"request"`
	EffectiveArguments AuditPayload `json:"effectiveArguments"`
	Result             AuditPayload `json:"result"`
}

type CallsStats struct {
	Since         time.Time                 `json:"since,omitempty"`
	Events        int                       `json:"events"`
	Calls         int                       `json:"calls"`
	Incomplete    int                       `json:"incomplete"`
	Skipped       int                       `json:"skippedMalformed"`
	PayloadRaw    int64                     `json:"payloadRawBytes"`
	PayloadStored int64                     `json:"payloadStoredBytes"`
	Outcomes      map[string]int            `json:"outcomes"`
	Clients       map[string]int            `json:"clients"`
	Servers       map[string]int            `json:"servers"`
	Tools         map[string]int            `json:"tools"`
	ServerTools   map[string]map[string]int `json:"serverTools"`
}

type CallsVerify struct {
	OK       bool     `json:"ok"`
	Events   int      `json:"events"`
	Payloads int      `json:"payloads"`
	Skipped  int      `json:"skippedMalformed"`
	Failures int      `json:"failures"`
	Issues   []string `json:"issues,omitempty"`
}

type CallsPrune struct {
	DryRun bool     `json:"dryRun"`
	Before string   `json:"before"`
	Days   int      `json:"days"`
	Bytes  int64    `json:"bytes"`
	Names  []string `json:"names,omitempty"`
}

type CallsKeyRotation struct {
	PreviousKeyID string `json:"previousKeyId"`
	KeyID         string `json:"keyId"`
	Enabled       bool   `json:"enabled"`
}

type CallsService struct{ c *Client }

func (s *CallsService) Status(ctx context.Context) (CallsStatus, error) {
	var out CallsStatus
	err := s.c.do(ctx, http.MethodGet, "/calls/status", nil, nil, &out)
	return out, err
}

type CallFilter struct {
	Since   time.Time
	Limit   int
	Cursor  string
	Query   string
	Client  string
	Server  string
	Tool    string
	Outcome string
}

func callQuery(f CallFilter) url.Values {
	q := url.Values{}
	if !f.Since.IsZero() {
		q.Set("since", f.Since.Format(time.RFC3339Nano))
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	if f.Cursor != "" {
		q.Set("cursor", f.Cursor)
	}
	if f.Query != "" {
		q.Set("query", f.Query)
	}
	if f.Client != "" {
		q.Set("client", f.Client)
	}
	if f.Server != "" {
		q.Set("server", f.Server)
	}
	if f.Tool != "" {
		q.Set("tool", f.Tool)
	}
	if f.Outcome != "" {
		q.Set("outcome", f.Outcome)
	}
	return q
}

func (s *CallsService) List(ctx context.Context, f CallFilter) (CallPage, error) {
	var out CallPage
	err := s.c.do(ctx, http.MethodGet, "/calls", callQuery(f), nil, &out)
	return out, err
}

func (s *CallsService) Get(ctx context.Context, id string) (CallDetail, error) {
	var out CallDetail
	err := s.c.do(ctx, http.MethodGet, "/calls/"+url.PathEscape(id), nil, nil, &out)
	return out, err
}

func (s *CallsService) Stats(ctx context.Context, since time.Time) (CallsStats, error) {
	q := url.Values{}
	if !since.IsZero() {
		q.Set("since", since.Format(time.RFC3339Nano))
	}
	var out CallsStats
	err := s.c.do(ctx, http.MethodGet, "/calls/stats", q, nil, &out)
	return out, err
}

func (s *CallsService) SetEnabled(ctx context.Context, enabled bool, expectedGeneration uint64) (CallsStatus, error) {
	var out CallsStatus
	err := s.c.doWrite(ctx, http.MethodPut, "/calls/enabled", nil, expectedGeneration,
		struct {
			Enabled bool `json:"enabled"`
		}{enabled}, &out)
	return out, err
}

func (s *CallsService) RotateKey(ctx context.Context, expectedGeneration uint64) (CallsKeyRotation, error) {
	var out CallsKeyRotation
	err := s.c.doWrite(ctx, http.MethodPost, "/calls/rotate-key", nil, expectedGeneration, struct{}{}, &out)
	return out, err
}

func (s *CallsService) Verify(ctx context.Context) (CallsVerify, error) {
	var out CallsVerify
	err := s.c.do(ctx, http.MethodPost, "/calls/verify", nil, struct{}{}, &out)
	return out, err
}

func (s *CallsService) Prune(ctx context.Context, dryRun bool) (CallsPrune, error) {
	var out CallsPrune
	err := s.c.do(ctx, http.MethodPost, "/calls/prune", nil,
		struct {
			DryRun bool `json:"dryRun"`
		}{dryRun}, &out)
	return out, err
}
