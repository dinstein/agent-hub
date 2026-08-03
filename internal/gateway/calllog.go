package gateway

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dinstein/agent-hub/internal/calllog"
	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/pipeline"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/router"
	"github.com/dinstein/agent-hub/internal/secrets"
)

const auditTextBound = 1024

// auditManager owns the gateway's current ledger store and the in-flight
// spans keyed by upstream request id. Reconfiguration never closes the old
// store immediately: a call that began under that policy must finish into
// the same ledger even if audit is disabled or rotated while it runs.
type auditManager struct {
	mu          sync.Mutex
	policy      registry.ResolvedCallsPolicy
	store       *calllog.Store
	unavailable error
	retired     []*calllog.Store
	spans       map[string]*auditSpan
}

type auditSpan struct {
	store   *calllog.Store
	policy  registry.ResolvedCallsPolicy
	started time.Time
	common  calllog.Event

	route    router.Route
	provider string
	gate     string
	code     string
}

func newAuditManager() *auditManager {
	return &auditManager{spans: map[string]*auditSpan{}}
}

func validateCallsPolicy(p registry.ResolvedCallsPolicy) error {
	if !p.Enabled {
		return nil
	}
	if p.Durability != "sync" && p.Durability != "write" {
		return fmt.Errorf("unknown durability %q", p.Durability)
	}
	switch p.ResultMode {
	case "none", "errors", "truncated", "full":
	default:
		return fmt.Errorf("unknown result mode %q", p.ResultMode)
	}
	if p.ResultBytes <= 0 || p.ResultBytes > calllog.MaxPayloadBytes {
		return fmt.Errorf("resultBytes %d is outside 1..%d", p.ResultBytes, calllog.MaxPayloadBytes)
	}
	if p.RetentionDays <= 0 || p.MaxBytes <= 0 || p.MinFreeBytes <= 0 {
		return errors.New("retentionDays, maxBytes and minFreeBytes must be positive")
	}
	if p.KeyID == "" {
		return errors.New("encryption key id is empty")
	}
	return nil
}

// syncAudit applies the current governance policy. Failure leaves the
// gateway usable for discovery but marks the ledger unavailable, so every
// subsequent tools/call is refused before execution until a valid policy is
// applied. That is the strict failure direction without taking tools/list
// and diagnostics down with it.
func (g *gateway) syncAudit() {
	if g.audit == nil {
		return
	}
	p := registry.ResolvedCallsPolicy{}
	if snap := g.snap.Load(); snap != nil {
		p = snap.Governance.V.ResolvedCalls()
	}

	g.audit.mu.Lock()
	if p == g.audit.policy && (g.audit.store != nil || g.audit.unavailable != nil || !p.Enabled) {
		g.audit.mu.Unlock()
		return
	}
	g.audit.mu.Unlock()

	if !p.Enabled {
		// Metadata without evidence: no key, no payloads, nothing that can
		// refuse a call. This is the ordinary state of an installation, and
		// before it existed that state recorded NOTHING — a hub whose ledger
		// was switched off could not say what any client had ever called.
		g.openMetadataOnly(p)
		return
	}
	if err := validateCallsPolicy(p); err != nil {
		g.swapStore(p, nil, fmt.Errorf("invalid audit policy: %w", err))
		g.log.Error("audit unavailable; tools/call will be blocked", "error", err)
		return
	}
	// Result capture changes do not require a new file handle or cipher. Reuse
	// the current store only when every storage invariant is unchanged; a
	// retention or pressure edit must take effect before the next write.
	g.audit.mu.Lock()
	if g.audit.store != nil && g.audit.unavailable == nil &&
		g.audit.policy.KeyID == p.KeyID && g.audit.policy.Durability == p.Durability &&
		g.audit.policy.RetentionDays == p.RetentionDays && g.audit.policy.MaxBytes == p.MaxBytes &&
		g.audit.policy.MinFreeBytes == p.MinFreeBytes {
		g.audit.policy = p
		g.audit.mu.Unlock()
		g.log.Info("audit policy applied", "results", p.ResultMode, "durability", p.Durability,
			"retention_days", p.RetentionDays, "max_bytes", p.MaxBytes, "key_id", p.KeyID)
		return
	}
	g.audit.mu.Unlock()
	if g.cfg.Secrets == nil {
		err := errors.New("secret resolver is unavailable")
		g.swapStore(p, nil, err)
		g.log.Error("audit unavailable; tools/call will be blocked", "error", err)
		return
	}
	encoded, ok, err := g.cfg.Secrets(g.lifeCtx, secrets.AuditEncryptionKeyRef(p.KeyID))
	if err == nil && !ok {
		// Compatibility with ledgers enabled before key-specific vault entries
		// existed. The id check below prevents a legacy current key from being
		// mistaken for the configured historical key.
		encoded, ok, err = g.cfg.Secrets(g.lifeCtx, secrets.AuditEncryptionRef())
	}
	if err != nil || !ok {
		if err == nil {
			err = errors.New("audit encryption key is missing")
		}
		g.swapStore(p, nil, err)
		g.log.Error("audit unavailable; tools/call will be blocked", "error", err)
		return
	}
	key, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil || len(key) != 32 {
		if err == nil {
			err = calllog.ErrBadKey
		}
		g.swapStore(p, nil, err)
		g.log.Error("audit unavailable; tools/call will be blocked", "error", err)
		return
	}
	keyID, err := calllog.KeyID(key)
	if err == nil && keyID != p.KeyID {
		err = fmt.Errorf("audit key id %q does not match configured %q", keyID, p.KeyID)
	}
	if err != nil {
		zeroBytes(key)
		g.swapStore(p, nil, err)
		g.log.Error("audit unavailable; tools/call will be blocked", "error", err)
		return
	}
	root, err := calllog.DefaultDir(g.resolver)
	if err != nil {
		zeroBytes(key)
		g.swapStore(p, nil, err)
		g.log.Error("audit unavailable; tools/call will be blocked", "error", err)
		return
	}
	store, err := calllog.Open(calllog.Options{
		Root: root, Key: key, KeyID: keyID, Durability: calllog.Durability(p.Durability),
		RetentionDays: p.RetentionDays, MaxBytes: p.MaxBytes, MinFreeBytes: p.MinFreeBytes,
	})
	zeroBytes(key)
	if err != nil {
		g.swapStore(p, nil, err)
		g.log.Error("audit unavailable; tools/call will be blocked", "error", err)
		return
	}
	g.swapStore(p, store, nil)
	g.log.Info("audit policy applied", "results", p.ResultMode, "durability", p.Durability,
		"retention_days", p.RetentionDays, "max_bytes", p.MaxBytes, "key_id", p.KeyID)
}

// openMetadataOnly opens the keyless store: bounded metadata lines and
// frames, fail-open in every direction. A failure here leaves the gateway
// serving, because a call that cannot be DESCRIBED is still a call that was
// authorized — the fail-closed rule belongs to evidence, which is what a
// governance answer is made of.
func (g *gateway) openMetadataOnly(p registry.ResolvedCallsPolicy) {
	root, err := calllog.DefaultDir(g.resolver)
	if err == nil {
		var store *calllog.Store
		store, err = calllog.Open(calllog.Options{
			Root: root, Durability: calllog.DurabilityWrite,
			RetentionDays: p.RetentionDays, MaxBytes: p.MaxBytes, MinFreeBytes: p.MinFreeBytes,
		})
		if err == nil {
			g.swapStore(p, store, nil)
			g.log.Info("recording call metadata", "retention_days", p.RetentionDays)
			return
		}
	}
	g.swapStore(p, nil, nil)
	g.log.Warn("call metadata will not be recorded; calls are unaffected", "error", err)
}

func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func (g *gateway) swapStore(p registry.ResolvedCallsPolicy, store *calllog.Store, unavailable error) {
	g.audit.swap(p, store, unavailable)
	// The frame switches follow the store, so a policy change cannot leave a
	// server writing frames into a ledger nobody reads. capture follows the
	// evidence tier: without a key there is nothing to seal a body with.
	var sink downstream.FrameSink
	if store != nil {
		sink = store
	}
	g.traces.setSink(sink, store.HasKey() && p.Enabled)
}

func (m *auditManager) swap(p registry.ResolvedCallsPolicy, store *calllog.Store, unavailable error) {
	m.mu.Lock()
	if m.store != nil && m.store != store {
		m.retired = append(m.retired, m.store)
	}
	m.policy, m.store, m.unavailable = p, store, unavailable
	m.mu.Unlock()
}

func (m *auditManager) close() error {
	m.mu.Lock()
	stores := append([]*calllog.Store(nil), m.retired...)
	if m.store != nil {
		stores = append(stores, m.store)
	}
	m.retired, m.store = nil, nil
	m.mu.Unlock()
	var errs []error
	for _, store := range stores {
		if err := store.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (g *gateway) auditBegin(req *mcp.Request) error {
	m := g.audit
	if m == nil {
		return nil
	}
	m.mu.Lock()
	p, store, unavailable := m.policy, m.store, m.unavailable
	m.mu.Unlock()
	// Only EVIDENCE fails closed. With it off, a store that could not be
	// opened costs the timeline a call and costs the call nothing.
	if p.Enabled && (unavailable != nil || store == nil) {
		if unavailable == nil {
			unavailable = errors.New("ledger store is unavailable")
		}
		return unavailable
	}
	if store == nil {
		return nil
	}
	callID, err := calllog.NewCallID()
	if err != nil {
		if p.Enabled {
			return err
		}
		return nil
	}
	started := time.Now().UTC()
	var request calllog.PayloadRef
	if store.HasKey() {
		request, err = store.PutPayload(started, callID, calllog.PayloadRequest, req.Params)
		if err != nil {
			return err
		}
	}
	face := g.cfg.Face
	if face == "" {
		face = "stdio"
	}
	face = boundedAuditText(face)
	common := calllog.Event{
		TS: started, CallID: callID, Client: boundedAuditText(g.cfg.ClientID),
		Session: boundedAuditText(face + ":" + g.cfg.ClientID), RequestID: boundedAuditText(req.ID.String()),
		Face: face, Protocol: boundedAuditText(g.auditRequestProtocol(req.Params)), PolicyRev: g.auditPolicyRev(),
		CallerTier: boundedAuditText(string(g.cfg.CallerTier)),
	}
	received := common
	received.Kind = calllog.EventReceived
	if store.HasKey() {
		received.Request = &request
	}
	if err := store.Append(received); err != nil {
		if p.Enabled {
			return err
		}
		return nil
	}
	span := &auditSpan{store: store, policy: p, started: started, common: common}
	m.mu.Lock()
	m.spans[req.ID.Key()] = span
	m.mu.Unlock()
	return nil
}

func (g *gateway) auditRoute(id mcp.ID, route router.Route, args json.RawMessage, provider string) error {
	span := g.auditSpan(id)
	if span == nil {
		return nil
	}
	span.route, span.provider = route, provider
	e := span.common
	e.TS, e.Kind = time.Now().UTC(), calllog.EventRouted
	e.Exposed, e.Server, e.Tool = boundedAuditText(span.common.Exposed), boundedAuditText(route.ServerID), boundedAuditText(route.RawTool)
	e.Provider = provider
	if span.store.HasKey() {
		ref, err := span.store.PutPayload(e.TS, span.common.CallID, calllog.PayloadEffectiveArgs, args)
		if err != nil {
			return err
		}
		e.EffectiveArgs = &ref
	}
	if err := span.store.Append(e); err != nil && span.policy.Enabled {
		return err
	}
	return nil
}

// auditCallID returns the ledger id of one in-flight request, or "" when
// nothing is being recorded for it.
func (g *gateway) auditCallID(id mcp.ID) string {
	if span := g.auditSpan(id); span != nil {
		return span.common.CallID
	}
	return ""
}

func (g *gateway) auditSetExposed(id mcp.ID, exposed string) {
	if span := g.auditSpan(id); span != nil {
		span.common.Exposed = boundedAuditText(exposed)
	}
}

func (g *gateway) auditMarkFailure(id mcp.ID, err error) {
	span := g.auditSpan(id)
	if span == nil {
		return
	}
	var blocked *pipeline.BlockedError
	if errors.As(err, &blocked) {
		span.gate, span.code = blocked.Gate, blocked.Code
	}
}

func (g *gateway) auditSpan(id mcp.ID) *auditSpan {
	if g.audit == nil {
		return nil
	}
	g.audit.mu.Lock()
	defer g.audit.mu.Unlock()
	return g.audit.spans[id.Key()]
}

func (g *gateway) auditFinishResponse(res *mcp.Response) error {
	if g.audit == nil || res == nil {
		return nil
	}
	g.audit.mu.Lock()
	span := g.audit.spans[res.ID.Key()]
	if span != nil {
		delete(g.audit.spans, res.ID.Key())
	}
	g.audit.mu.Unlock()
	if span == nil {
		return nil
	}
	return span.finishResponse(res)
}

func (g *gateway) auditFinishCancelled(id mcp.ID) {
	if g.audit == nil {
		return
	}
	g.audit.mu.Lock()
	span := g.audit.spans[id.Key()]
	if span != nil {
		delete(g.audit.spans, id.Key())
	}
	g.audit.mu.Unlock()
	if span == nil {
		return
	}
	e := span.finished("cancelled", nil)
	if err := span.store.Append(e); err != nil {
		g.log.Error("audit cancellation record failed", "call_id", span.common.CallID, "error", err)
	}
}

func (s *auditSpan) finishResponse(res *mcp.Response) error {
	outcome := "success"
	var payload []byte
	toolError := false
	if res.Error != nil {
		outcome = "protocol_error"
		if s.gate != "" {
			outcome = "denied"
		} else if res.Error.Code == codeRetryBusy {
			outcome = "busy"
		}
		payload, _ = json.Marshal(res.Error)
	} else {
		payload = res.Result
		var result mcp.CallResult
		if json.Unmarshal(res.Result, &result) == nil && result.IsError {
			outcome, toolError = "tool_error", true
		}
	}
	e := s.finished(outcome, res.Error)
	e.ToolError, e.ResultMode, e.ResultBytes = toolError, s.policy.ResultMode, len(payload)

	capture := false
	stored := payload
	switch s.policy.ResultMode {
	case "none":
	case "errors":
		capture = res.Error != nil || toolError
	case "truncated":
		capture = true
		if len(stored) > s.policy.ResultBytes {
			stored = stored[:s.policy.ResultBytes]
			e.ResultCut = true
		}
	case "full":
		capture = true
	default:
		return fmt.Errorf("audit: unknown result mode %q", s.policy.ResultMode)
	}
	if capture && s.store.HasKey() {
		ref, err := s.store.PutPayload(e.TS, s.common.CallID, calllog.PayloadResult, stored)
		if err != nil {
			return err
		}
		e.Result = &ref
		if e.ResultCut {
			e.ResultCapture = "truncated"
		} else {
			e.ResultCapture = "full"
		}
	} else {
		e.ResultCapture = "none"
	}
	if err := s.store.Append(e); err != nil && s.policy.Enabled {
		return err
	}
	return nil
}

func (s *auditSpan) finished(outcome string, rpcErr *mcp.Error) calllog.Event {
	e := s.common
	e.TS, e.Kind, e.Outcome = time.Now().UTC(), calllog.EventFinished, outcome
	e.DurationMs = time.Since(s.started).Milliseconds()
	e.Server, e.Tool, e.Provider = boundedAuditText(s.route.ServerID), boundedAuditText(s.route.RawTool), s.provider
	e.Gate, e.Code = s.gate, s.code
	if rpcErr != nil {
		if e.Code == "" {
			e.Code = strconv.Itoa(rpcErr.Code)
		}
		e.Error = boundedAuditText(rpcErr.Message)
	}
	return e
}

func boundedAuditText(s string) string {
	s = strings.ToValidUTF8(s, "�")
	if len(s) <= auditTextBound {
		return s
	}
	return s[:auditTextBound]
}

func (g *gateway) auditPolicyRev() uint64 {
	if snap := g.snap.Load(); snap != nil {
		return snap.Generation
	}
	return 0
}

func (g *gateway) auditProtocol() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.protocol != "" {
		return g.protocol
	}
	if g.stateless {
		return mcp.Version2026
	}
	return "stateful"
}

func (g *gateway) auditRequestProtocol(params json.RawMessage) string {
	var probe struct {
		Meta *mcp.RequestMeta `json:"_meta"`
	}
	if json.Unmarshal(params, &probe) == nil && probe.Meta != nil && probe.Meta.ProtocolVersion != "" {
		return probe.Meta.ProtocolVersion
	}
	return g.auditProtocol()
}
