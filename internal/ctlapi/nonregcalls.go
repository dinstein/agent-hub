package ctlapi

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dinstein/agent-hub/api"
	"github.com/dinstein/agent-hub/internal/calllog"
	"github.com/dinstein/agent-hub/internal/confops"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/secrets"
)

const auditPayloadPreviewBytes = 512 << 10

func (s *Server) auditStatus() (api.CallsStatus, error) {
	snap := s.opts.Registry.Snapshot()
	p := snap.Governance.V.ResolvedCalls()
	u, err := calllog.Inspect(s.opts.NonRegistry.CallsRoot)
	if err != nil {
		return api.CallsStatus{}, err
	}
	return api.CallsStatus{
		Generation: snap.Generation, Enabled: p.Enabled, Arguments: "full",
		Results: p.ResultMode, ResultBytes: p.ResultBytes, Durability: p.Durability,
		RetentionDays: p.RetentionDays, MaxBytes: p.MaxBytes,
		MinFreeBytes: p.MinFreeBytes, Pressure: "block", KeyID: p.KeyID,
		Storage: api.AuditUsage{Bytes: u.Bytes, Days: u.Days, EventFiles: u.EventFiles, PackFiles: u.PackFiles},
	}, nil
}

func (s *Server) handleCallsStatus(w http.ResponseWriter, r *http.Request) {
	out, err := s.auditStatus()
	if err != nil {
		s.writeAuditError(w, r, err)
		return
	}
	writeOK(w, http.StatusOK, out)
}

func auditSince(r *http.Request) (time.Time, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("since"))
	if raw == "" {
		return time.Now().Add(-24 * time.Hour), true
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	return t, err == nil
}

func eventIntoSummary(out *api.CallSummary, e calllog.Event) {
	if out.CallID == "" {
		out.CallID, out.Time = e.CallID, e.TS
	}
	if e.Kind == calllog.EventReceived {
		out.Time, out.Client, out.Face, out.ExposedTool = e.TS, e.Client, e.Face, e.Exposed
	}
	if e.Server != "" {
		out.Server = e.Server
	}
	if e.Tool != "" {
		out.Tool = e.Tool
	}
	if e.Kind == calllog.EventFinished {
		out.Complete = true
		out.Outcome, out.DurationMs, out.Code = e.Outcome, e.DurationMs, e.Code
		out.ResultCapture = e.ResultCapture
	}
}

func auditMatches(c api.CallSummary, q map[string]string) bool {
	if query := strings.ToLower(strings.TrimSpace(q["query"])); query != "" {
		values := []string{c.CallID, c.Client, c.Face, c.ExposedTool, c.Server, c.Tool, c.Outcome, c.Code}
		matched := false
		for _, value := range values {
			if strings.Contains(strings.ToLower(value), query) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return (q["client"] == "" || c.Client == q["client"]) &&
		(q["server"] == "" || c.Server == q["server"]) &&
		(q["tool"] == "" || c.Tool == q["tool"] || c.ExposedTool == q["tool"]) &&
		(q["outcome"] == "" || c.Outcome == q["outcome"])
}

type auditListCursor struct {
	time   time.Time
	callID string
}

func encodeCallsListCursor(row api.CallSummary) string {
	raw := row.Time.Format(time.RFC3339Nano) + "\n" + row.CallID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCallsListCursor(raw string) (auditListCursor, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return auditListCursor{}, fmt.Errorf("decode cursor: %w", err)
	}
	timestamp, callID, ok := strings.Cut(string(decoded), "\n")
	if !ok || callID == "" {
		return auditListCursor{}, fmt.Errorf("cursor has invalid shape")
	}
	t, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return auditListCursor{}, fmt.Errorf("cursor has invalid timestamp: %w", err)
	}
	return auditListCursor{time: t, callID: callID}, nil
}

func auditRowAfterCursor(row api.CallSummary, cursor auditListCursor) bool {
	return row.Time.Before(cursor.time) || (row.Time.Equal(cursor.time) && row.CallID < cursor.callID)
}

func (s *Server) handleCallPage(w http.ResponseWriter, r *http.Request) {
	since, ok := auditSince(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "since must be an RFC3339 timestamp", "choose a valid time range", requestIDFrom(r.Context()))
		return
	}
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		var err error
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 1000 {
			writeErr(w, http.StatusBadRequest, CodeBadRequest, "limit must be from 1 through 1000", "choose a smaller page size", requestIDFrom(r.Context()))
			return
		}
	}
	var cursor auditListCursor
	if raw := strings.TrimSpace(r.URL.Query().Get("cursor")); raw != "" {
		var err error
		cursor, err = decodeCallsListCursor(raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, CodeBadRequest, "cursor is invalid", "return to the first page", requestIDFrom(r.Context()))
			return
		}
	}
	byID := map[string]*api.CallSummary{}
	skipped, err := calllog.ScanEventsSince(s.opts.NonRegistry.CallsRoot, since, func(e calllog.Event) error {
		row := byID[e.CallID]
		if row == nil {
			row = &api.CallSummary{}
			byID[e.CallID] = row
		}
		eventIntoSummary(row, e)
		return nil
	})
	if err != nil {
		s.writeAuditError(w, r, err)
		return
	}
	filters := map[string]string{
		"query": r.URL.Query().Get("query"), "client": r.URL.Query().Get("client"),
		"server": r.URL.Query().Get("server"), "tool": r.URL.Query().Get("tool"),
		"outcome": r.URL.Query().Get("outcome"),
	}
	rows := make([]api.CallSummary, 0, len(byID))
	for _, row := range byID {
		if auditMatches(*row, filters) {
			rows = append(rows, *row)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Time.Equal(rows[j].Time) {
			return rows[i].CallID > rows[j].CallID
		}
		return rows[i].Time.After(rows[j].Time)
	})
	total := len(rows)
	start := 0
	if !cursor.time.IsZero() {
		start = sort.Search(len(rows), func(i int) bool { return auditRowAfterCursor(rows[i], cursor) })
	}
	end := min(start+limit, len(rows))
	page := rows[start:end]
	nextCursor := ""
	if end < len(rows) && len(page) > 0 {
		nextCursor = encodeCallsListCursor(page[len(page)-1])
	}
	writeOK(w, http.StatusOK, api.CallPage{
		Since: since, Calls: page, Total: total, NextCursor: nextCursor, Skipped: skipped,
	})
}

func auditEventView(e calllog.Event) api.AuditEvent {
	return api.AuditEvent{Time: e.TS, Event: string(e.Kind), RequestID: e.RequestID,
		Session: e.Session, PolicyRev: e.PolicyRev, Server: e.Server, Tool: e.Tool,
		Outcome: e.Outcome, DurationMs: e.DurationMs, Gate: e.Gate, Rule: e.Rule,
		Code: e.Code, Error: e.Error, ToolError: e.ToolError}
}

type auditKeyCache struct {
	ctx   context.Context
	vault AuditKeyVault
	keys  map[string][]byte
}

func (c *auditKeyCache) close() {
	for _, key := range c.keys {
		clear(key)
	}
}

func (c *auditKeyCache) get(id string) ([]byte, error) {
	if key := c.keys[id]; key != nil {
		return key, nil
	}
	if len(id) != 16 {
		return nil, fmt.Errorf("invalid audit key id %q", id)
	}
	encoded, ok, err := c.vault.Get(c.ctx, secrets.AuditEncryptionKeyRef(id))
	if err == nil && !ok {
		encoded, ok, err = c.vault.Get(c.ctx, secrets.AuditEncryptionRef())
	}
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("audit encryption key %q not found", id)
	}
	key, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("stored audit encryption key is invalid")
	}
	got, err := calllog.KeyID(key)
	if err != nil || got != id {
		clear(key)
		return nil, fmt.Errorf("stored audit key does not match id %q", id)
	}
	c.keys[id] = key
	return key, nil
}

func payloadView(raw []byte) api.AuditPayload {
	out := api.AuditPayload{Bytes: len(raw)}
	if len(raw) > auditPayloadPreviewBytes {
		raw, out.Truncated = raw[:auditPayloadPreviewBytes], true
	}
	out.Text = string(raw)
	return out
}

func (s *Server) handleAuditCall(w http.ResponseWriter, r *http.Request, id string) {
	var events []calllog.Event
	_, err := calllog.ScanEvents(s.opts.NonRegistry.CallsRoot, func(e calllog.Event) error {
		if e.CallID == id {
			events = append(events, e)
		}
		return nil
	})
	if err != nil {
		s.writeAuditError(w, r, err)
		return
	}
	if len(events) == 0 {
		writeNotFound(w, r)
		return
	}
	out := api.CallDetail{}
	for _, e := range events {
		eventIntoSummary(&out.CallSummary, e)
		if e.Kind == calllog.EventFinished {
			out.Error = e.Error
		}
		out.Events = append(out.Events, auditEventView(e))
	}
	keys := &auditKeyCache{ctx: r.Context(), vault: s.opts.NonRegistry.CallsKeys, keys: map[string][]byte{}}
	defer keys.close()
	for _, e := range events {
		for _, item := range []struct {
			ref *calllog.PayloadRef
			dst *api.AuditPayload
		}{{e.Request, &out.Request}, {e.EffectiveArgs, &out.EffectiveArguments}, {e.Result, &out.Result}} {
			if item.ref == nil {
				continue
			}
			key, keyErr := keys.get(item.ref.KeyID)
			if keyErr != nil {
				s.writeAuditError(w, r, keyErr)
				return
			}
			raw, readErr := calllog.ReadPayload(s.opts.NonRegistry.CallsRoot, *item.ref, key)
			if readErr != nil {
				s.writeAuditError(w, r, readErr)
				return
			}
			*item.dst = payloadView(raw)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeOK(w, http.StatusOK, out)
}

func (s *Server) handleCallsStats(w http.ResponseWriter, r *http.Request) {
	since, ok := auditSince(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "since must be an RFC3339 timestamp", "choose a valid time range", requestIDFrom(r.Context()))
		return
	}
	out := api.CallsStats{
		Since: since, Outcomes: map[string]int{}, Clients: map[string]int{},
		Servers: map[string]int{}, Tools: map[string]int{}, ServerTools: map[string]map[string]int{},
	}
	received, finished := map[string]bool{}, map[string]bool{}
	var err error
	out.Skipped, err = calllog.ScanEventsSince(s.opts.NonRegistry.CallsRoot, since, func(e calllog.Event) error {
		out.Events++
		if e.Kind == calllog.EventReceived {
			received[e.CallID] = true
			out.Clients[e.Client]++
		}
		if e.Kind == calllog.EventFinished {
			finished[e.CallID] = true
			out.Outcomes[e.Outcome]++
			if e.Server != "" {
				out.Servers[e.Server]++
			}
			if e.Tool != "" {
				out.Tools[e.Tool]++
			}
			if e.Server != "" && e.Tool != "" {
				if out.ServerTools[e.Server] == nil {
					out.ServerTools[e.Server] = map[string]int{}
				}
				out.ServerTools[e.Server][e.Tool]++
			}
		}
		for _, ref := range []*calllog.PayloadRef{e.Request, e.EffectiveArgs, e.Result} {
			if ref != nil {
				out.PayloadRaw += int64(ref.RawBytes)
				out.PayloadStored += int64(ref.StoredBytes)
			}
		}
		return nil
	})
	if err != nil {
		s.writeAuditError(w, r, err)
		return
	}
	out.Calls = len(received)
	for id := range received {
		if !finished[id] {
			out.Incomplete++
		}
	}
	writeOK(w, http.StatusOK, out)
}

func (s *Server) handleAuditEnabled(w http.ResponseWriter, r *http.Request) {
	body, ok := readAdminBody(w, r)
	if !ok {
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if !decodeAdminBody(w, r, body, &req) {
		return
	}
	pre, ok := adminPrecondition(w, r, body)
	if !ok {
		return
	}
	keyID := ""
	if req.Enabled {
		key, err := s.loadOrCreateAuditKey(r.Context())
		if err != nil {
			s.writeAuditError(w, r, err)
			return
		}
		keyID, err = calllog.KeyID(key)
		clear(key)
		if err != nil {
			s.writeAuditError(w, r, err)
			return
		}
	}
	res, err := confops.SetCallsEnabled(r.Context(), s.opts.Registry, req.Enabled, keyID, pre)
	if err != nil {
		s.writeOpsError(w, r, err)
		return
	}
	s.publishRegistryChange(registry.DocGovernance, res.Generation)
	out, err := s.auditStatus()
	if err != nil {
		s.writeAuditError(w, r, err)
		return
	}
	writeOK(w, http.StatusOK, out)
}

func (s *Server) loadOrCreateAuditKey(ctx context.Context) ([]byte, error) {
	v := s.opts.NonRegistry.CallsKeys
	encoded, ok, err := v.Get(ctx, secrets.AuditEncryptionRef())
	if err != nil {
		return nil, err
	}
	var key []byte
	if ok {
		key, err = base64.RawStdEncoding.DecodeString(encoded)
		if err != nil || len(key) != 32 {
			return nil, fmt.Errorf("stored audit encryption key is invalid")
		}
	} else {
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, err
		}
		encoded = base64.RawStdEncoding.EncodeToString(key)
		if err = v.Set(ctx, secrets.AuditEncryptionRef(), encoded); err != nil {
			clear(key)
			return nil, err
		}
	}
	id, err := calllog.KeyID(key)
	if err != nil {
		clear(key)
		return nil, err
	}
	if err = v.Set(ctx, secrets.AuditEncryptionKeyRef(id), encoded); err != nil {
		clear(key)
		return nil, err
	}
	return key, nil
}

func (s *Server) handleAuditRotateKey(w http.ResponseWriter, r *http.Request) {
	body, ok := readAdminBody(w, r)
	if !ok {
		return
	}
	pre, ok := adminPrecondition(w, r, body)
	if !ok {
		return
	}
	previous := s.opts.Registry.Snapshot().Governance.V.ResolvedCalls()
	if previous.KeyID == "" {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "audit has no current key", "enable audit first", requestIDFrom(r.Context()))
		return
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		s.writeAuditError(w, r, err)
		return
	}
	defer clear(key)
	id, err := calllog.KeyID(key)
	if err != nil {
		s.writeAuditError(w, r, err)
		return
	}
	encoded := base64.RawStdEncoding.EncodeToString(key)
	if err = s.opts.NonRegistry.CallsKeys.Set(r.Context(), secrets.AuditEncryptionKeyRef(id), encoded); err != nil {
		s.writeAuditError(w, r, err)
		return
	}
	res, err := confops.SetAuditKeyID(r.Context(), s.opts.Registry, id, pre)
	if err != nil {
		s.writeOpsError(w, r, err)
		return
	}
	if err = s.opts.NonRegistry.CallsKeys.Set(r.Context(), secrets.AuditEncryptionRef(), encoded); err != nil {
		s.writeAuditError(w, r, err)
		return
	}
	s.publishRegistryChange(registry.DocGovernance, res.Generation)
	writeOK(w, http.StatusOK, api.CallsKeyRotation{PreviousKeyID: previous.KeyID, KeyID: id, Enabled: res.Policy.Enabled})
}

func (s *Server) handleCallsVerify(w http.ResponseWriter, r *http.Request) {
	keys := &auditKeyCache{ctx: r.Context(), vault: s.opts.NonRegistry.CallsKeys, keys: map[string][]byte{}}
	defer keys.close()
	out := api.CallsVerify{OK: true}
	add := func(issue string) {
		out.OK = false
		out.Failures++
		if len(out.Issues) < 50 {
			out.Issues = append(out.Issues, issue)
		}
	}
	var err error
	out.Skipped, err = calllog.ScanEvents(s.opts.NonRegistry.CallsRoot, func(e calllog.Event) error {
		out.Events++
		key, eerr := keys.get(e.KeyID)
		if eerr != nil {
			add(fmt.Sprintf("event %s/%s: %v", e.CallID, e.Kind, eerr))
			return nil
		}
		if eerr = calllog.VerifyEvent(e, key); eerr != nil {
			add(fmt.Sprintf("event %s/%s: %v", e.CallID, e.Kind, eerr))
		}
		for _, item := range []struct {
			ref  *calllog.PayloadRef
			kind calllog.PayloadKind
		}{{e.Request, calllog.PayloadRequest}, {e.EffectiveArgs, calllog.PayloadEffectiveArgs}, {e.Result, calllog.PayloadResult}} {
			if item.ref == nil {
				continue
			}
			out.Payloads++
			pkey, perr := keys.get(item.ref.KeyID)
			if perr == nil {
				perr = calllog.VerifyPayload(s.opts.NonRegistry.CallsRoot, *item.ref, pkey, e.CallID, item.kind)
			}
			if perr != nil {
				add(fmt.Sprintf("payload %s/%s: %v", e.CallID, item.kind, perr))
			}
		}
		return nil
	})
	if err != nil {
		s.writeAuditError(w, r, err)
		return
	}
	if out.Skipped > 0 {
		add(fmt.Sprintf("%d malformed event line(s)", out.Skipped))
	}
	writeOK(w, http.StatusOK, out)
}

func (s *Server) handleCallsPrune(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DryRun bool `json:"dryRun"`
	}
	body, ok := readAdminBody(w, r)
	if !ok || !decodeAdminBody(w, r, body, &req) {
		return
	}
	p := s.opts.Registry.Snapshot().Governance.V.ResolvedCalls()
	cutoff := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -(p.RetentionDays - 1))
	res, err := calllog.Prune(s.opts.NonRegistry.CallsRoot, cutoff, req.DryRun)
	if err != nil {
		s.writeAuditError(w, r, err)
		return
	}
	writeOK(w, http.StatusOK, api.CallsPrune{DryRun: req.DryRun, Before: cutoff.Format("2006-01-02"), Days: res.Days, Bytes: res.Bytes, Names: res.Names})
}

func (s *Server) writeAuditError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("audit control operation failed", "error", err)
	writeErr(w, http.StatusInternalServerError, CodeInternal, "audit operation failed", "check the daemon log and ledger storage", requestIDFrom(r.Context()))
}
