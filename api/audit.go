package api

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// AuditStatus is the effective access-ledger policy and its current storage
// footprint. KeyID is public key metadata; encryption key material never
// crosses the control plane.
type AuditStatus struct {
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

// AuditCallSummary joins one call's immutable lifecycle into a list-safe,
// metadata-only row. Payload references are deliberately not exposed.
type AuditCallSummary struct {
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

type AuditCalls struct {
	Since   time.Time          `json:"since,omitempty"`
	Calls   []AuditCallSummary `json:"calls"`
	Skipped int                `json:"skippedMalformed"`
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
}

// AuditPayload is decrypted only for one explicitly selected call. Truncated
// means the GUI preview is shorter than the authenticated ledger payload.
type AuditPayload struct {
	Text      string `json:"text,omitempty"`
	Bytes     int    `json:"bytes,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

type AuditCallDetail struct {
	AuditCallSummary
	Events             []AuditEvent `json:"events"`
	Error              string       `json:"error,omitempty"`
	Request            AuditPayload `json:"request"`
	EffectiveArguments AuditPayload `json:"effectiveArguments"`
	Result             AuditPayload `json:"result"`
}

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

type AuditVerify struct {
	OK       bool     `json:"ok"`
	Events   int      `json:"events"`
	Payloads int      `json:"payloads"`
	Skipped  int      `json:"skippedMalformed"`
	Failures int      `json:"failures"`
	Issues   []string `json:"issues,omitempty"`
}

type AuditPrune struct {
	DryRun bool     `json:"dryRun"`
	Before string   `json:"before"`
	Days   int      `json:"days"`
	Bytes  int64    `json:"bytes"`
	Names  []string `json:"names,omitempty"`
}

type AuditKeyRotation struct {
	PreviousKeyID string `json:"previousKeyId"`
	KeyID         string `json:"keyId"`
	Enabled       bool   `json:"enabled"`
}

type AuditService struct{ c *Client }

func (s *AuditService) Status(ctx context.Context) (AuditStatus, error) {
	var out AuditStatus
	err := s.c.do(ctx, http.MethodGet, "/audit/status", nil, nil, &out)
	return out, err
}

type AuditCallFilter struct {
	Since   time.Time
	Limit   int
	Client  string
	Server  string
	Tool    string
	Outcome string
}

func auditQuery(f AuditCallFilter) url.Values {
	q := url.Values{}
	if !f.Since.IsZero() {
		q.Set("since", f.Since.Format(time.RFC3339Nano))
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
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

func (s *AuditService) Calls(ctx context.Context, f AuditCallFilter) (AuditCalls, error) {
	var out AuditCalls
	err := s.c.do(ctx, http.MethodGet, "/audit/calls", auditQuery(f), nil, &out)
	return out, err
}

func (s *AuditService) Call(ctx context.Context, id string) (AuditCallDetail, error) {
	var out AuditCallDetail
	err := s.c.do(ctx, http.MethodGet, "/audit/calls/"+url.PathEscape(id), nil, nil, &out)
	return out, err
}

func (s *AuditService) Stats(ctx context.Context, since time.Time) (AuditStats, error) {
	q := url.Values{}
	if !since.IsZero() {
		q.Set("since", since.Format(time.RFC3339Nano))
	}
	var out AuditStats
	err := s.c.do(ctx, http.MethodGet, "/audit/stats", q, nil, &out)
	return out, err
}

func (s *AuditService) SetEnabled(ctx context.Context, enabled bool, expectedGeneration uint64) (AuditStatus, error) {
	var out AuditStatus
	err := s.c.doWrite(ctx, http.MethodPut, "/audit/enabled", nil, expectedGeneration,
		struct {
			Enabled bool `json:"enabled"`
		}{enabled}, &out)
	return out, err
}

func (s *AuditService) RotateKey(ctx context.Context, expectedGeneration uint64) (AuditKeyRotation, error) {
	var out AuditKeyRotation
	err := s.c.doWrite(ctx, http.MethodPost, "/audit/rotate-key", nil, expectedGeneration, struct{}{}, &out)
	return out, err
}

func (s *AuditService) Verify(ctx context.Context) (AuditVerify, error) {
	var out AuditVerify
	err := s.c.do(ctx, http.MethodPost, "/audit/verify", nil, struct{}{}, &out)
	return out, err
}

func (s *AuditService) Prune(ctx context.Context, dryRun bool) (AuditPrune, error) {
	var out AuditPrune
	err := s.c.do(ctx, http.MethodPost, "/audit/prune", nil,
		struct {
			DryRun bool `json:"dryRun"`
		}{dryRun}, &out)
	return out, err
}
