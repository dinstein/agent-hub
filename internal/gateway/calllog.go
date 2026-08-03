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

const ledgerTextBound = 1024

// ledgerManager owns the gateway's current ledger store and the in-flight
// spans keyed by upstream request id. Reconfiguration never closes the old
// store immediately: a call that began under that policy must finish into
// the same ledger even if audit is disabled or rotated while it runs.
type ledgerManager struct {
	mu          sync.Mutex
	policy      registry.ResolvedCallsPolicy
	store       *calllog.Store
	unavailable error
	retired     []*calllog.Store
	spans       map[string]*ledgerSpan
}

type ledgerSpan struct {
	store   *calllog.Store
	policy  registry.ResolvedCallsPolicy
	started time.Time
	common  calllog.Event

	route    router.Route
	provider string
	gate     string
	code     string
	// governed marks the one method whose record is a precondition for
	// running it. Everything else is recorded on this same path and fails
	// open, so a ledger problem cannot break a session's handshake.
	governed bool
}

func newLedgerManager() *ledgerManager {
	return &ledgerManager{spans: map[string]*ledgerSpan{}}
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
	encoded, ok, err := g.cfg.Secrets(g.lifeCtx, secrets.CallsEncryptionKeyRef(p.KeyID))
	if err == nil && !ok {
		// Compatibility with ledgers enabled before key-specific vault entries
		// existed. The id check below prevents a legacy current key from being
		// mistaken for the configured historical key.
		encoded, ok, err = g.cfg.Secrets(g.lifeCtx, secrets.CallsEncryptionRef())
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

func (m *ledgerManager) swap(p registry.ResolvedCallsPolicy, store *calllog.Store, unavailable error) {
	m.mu.Lock()
	if m.store != nil && m.store != store {
		m.retired = append(m.retired, m.store)
	}
	m.policy, m.store, m.unavailable = p, store, unavailable
	m.mu.Unlock()
}

func (m *ledgerManager) close() error {
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

func (g *gateway) ledgerBegin(req *mcp.Request) error {
	m := g.audit
	if m == nil {
		return nil
	}
	m.mu.Lock()
	p, store, unavailable := m.policy, m.store, m.unavailable
	m.mu.Unlock()
	// Only EVIDENCE fails closed, and only for tools/call: that is the
	// governed method, and it is the one that must not run unrecorded.
	// Everything else a client asks of agenthub is recorded on the same path
	// — same span, same payload capture, same finish — but a ledger that
	// cannot take it costs the timeline a line rather than breaking the
	// session's `initialize`.
	governed := req.Method == mcp.MethodToolsCall
	if governed && p.Enabled && (unavailable != nil || store == nil) {
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
		if governed && p.Enabled {
			return err
		}
		return nil
	}
	started := time.Now().UTC()
	var request calllog.PayloadRef
	if store.HasKey() {
		request, err = store.PutPayload(started, callID, calllog.PayloadRequest, req.Params)
		if err != nil {
			if governed {
				return err
			}
			// An ungoverned request keeps its metadata line; losing the
			// params costs the reader the body, not the fact.
			request = calllog.PayloadRef{}
		}
	}
	common := g.ledgerCommon(callID, req, started)
	received := common
	received.Kind = calllog.EventReceived
	if store.HasKey() && request.Length > 0 {
		received.Request = &request
	}
	if err := store.Append(received); err != nil {
		if governed && p.Enabled {
			return err
		}
		return nil
	}
	span := &ledgerSpan{store: store, policy: p, started: started, common: common, governed: governed}
	m.mu.Lock()
	m.spans[req.ID.Key()] = span
	m.mu.Unlock()
	return nil
}

func (g *gateway) ledgerRoute(id mcp.ID, route router.Route, args json.RawMessage, provider string) error {
	span := g.ledgerSpan(id)
	if span == nil {
		return nil
	}
	span.route, span.provider = route, provider
	e := span.common
	e.TS, e.Kind = time.Now().UTC(), calllog.EventRouted
	e.Exposed, e.Server, e.Tool = boundedLedgerText(span.common.Exposed), boundedLedgerText(route.ServerID), boundedLedgerText(route.RawTool)
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

// ledgerCommon is the identity every record of one upstream request carries:
// who asked, over which face, under which policy generation, and — since
// every method is recorded now, not only tools/call — which method it was.
//
// One function because the two paths that build it were one copy apart, and
// the copy that drifts is the one that leaves a record nobody can join.
func (g *gateway) ledgerCommon(callID string, req *mcp.Request, started time.Time) calllog.Event {
	face := g.cfg.Face
	if face == "" {
		face = "stdio"
	}
	face = boundedLedgerText(face)
	return calllog.Event{
		TS: started, CallID: callID, Client: boundedLedgerText(g.cfg.ClientID),
		Session:   boundedLedgerText(face + ":" + g.cfg.ClientID),
		RequestID: boundedLedgerText(req.ID.String()), Face: face,
		Method:     boundedLedgerText(req.Method),
		Protocol:   boundedLedgerText(g.ledgerRequestProtocol(req.Params)),
		PolicyRev:  g.ledgerPolicyRev(),
		CallerTier: boundedLedgerText(string(g.cfg.CallerTier)),
	}
}

// ledgerCallID returns the ledger id of one in-flight request, or "" when
// nothing is being recorded for it.
func (g *gateway) ledgerCallID(id mcp.ID) string {
	if span := g.ledgerSpan(id); span != nil {
		return span.common.CallID
	}
	return ""
}

func (g *gateway) ledgerSetExposed(id mcp.ID, exposed string) {
	if span := g.ledgerSpan(id); span != nil {
		span.common.Exposed = boundedLedgerText(exposed)
	}
}

// ledgerSetSurface records which of agenthub's own surfaces the name reached.
// It is set after classification and before anything acts on it, so a call
// that fails a gate still says what it was asking for.
func (g *gateway) ledgerSetSurface(id mcp.ID, surface string) {
	if span := g.ledgerSpan(id); span != nil {
		span.common.Surface = boundedLedgerText(surface)
	}
}

func (g *gateway) ledgerMarkFailure(id mcp.ID, err error) {
	span := g.ledgerSpan(id)
	if span == nil {
		return
	}
	var blocked *pipeline.BlockedError
	if errors.As(err, &blocked) {
		span.gate, span.code = blocked.Gate, blocked.Code
	}
}

func (g *gateway) ledgerSpan(id mcp.ID) *ledgerSpan {
	if g.audit == nil {
		return nil
	}
	g.audit.mu.Lock()
	defer g.audit.mu.Unlock()
	return g.audit.spans[id.Key()]
}

func (g *gateway) ledgerFinishResponse(res *mcp.Response) error {
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

func (g *gateway) ledgerFinishCancelled(id mcp.ID) {
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

func (s *ledgerSpan) finishResponse(res *mcp.Response) error {
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
			if s.governed {
				return err
			}
			// Same trade as the request half: an ungoverned record keeps its
			// line without the body.
			ref = calllog.PayloadRef{}
		}
		if ref.Length > 0 {
			e.Result = &ref
		}
		if e.ResultCut {
			e.ResultCapture = "truncated"
		} else {
			e.ResultCapture = "full"
		}
	} else {
		e.ResultCapture = "none"
	}
	if err := s.store.Append(e); err != nil && s.governed && s.policy.Enabled {
		return err
	}
	return nil
}

func (s *ledgerSpan) finished(outcome string, rpcErr *mcp.Error) calllog.Event {
	e := s.common
	e.TS, e.Kind, e.Outcome = time.Now().UTC(), calllog.EventFinished, outcome
	e.DurationMs = time.Since(s.started).Milliseconds()
	e.Server, e.Tool, e.Provider = boundedLedgerText(s.route.ServerID), boundedLedgerText(s.route.RawTool), s.provider
	e.Gate, e.Code = s.gate, s.code
	if rpcErr != nil {
		if e.Code == "" {
			e.Code = strconv.Itoa(rpcErr.Code)
		}
		e.Error = boundedLedgerText(rpcErr.Message)
	}
	return e
}

func boundedLedgerText(s string) string {
	s = strings.ToValidUTF8(s, "�")
	if len(s) <= ledgerTextBound {
		return s
	}
	return s[:ledgerTextBound]
}

func (g *gateway) ledgerPolicyRev() uint64 {
	if snap := g.snap.Load(); snap != nil {
		return snap.Generation
	}
	return 0
}

func (g *gateway) ledgerProtocol() string {
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

func (g *gateway) ledgerRequestProtocol(params json.RawMessage) string {
	var probe struct {
		Meta *mcp.RequestMeta `json:"_meta"`
	}
	if json.Unmarshal(params, &probe) == nil && probe.Meta != nil && probe.Meta.ProtocolVersion != "" {
		return probe.Meta.ProtocolVersion
	}
	return g.ledgerProtocol()
}
