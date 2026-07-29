package session

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/dinstein/agent-hub/internal/event"
	"github.com/dinstein/agent-hub/internal/scope"
)

// Bus topics published by the manager. Payload types are documented per
// topic; all payloads are immutable snapshots.
const (
	// TopicOpened fires on Register/OpenHTTP. Key = session ID; payload Info.
	TopicOpened event.Topic = "session.opened"
	// TopicClosed fires on Close (explicit or reaped). Key = session ID;
	// payload Closed.
	TopicClosed event.Topic = "session.closed"
	// TopicOverlay fires after a successful Mutate. Key = session ID;
	// payload OverlayChanged. Subscribers invalidate the scope resolver
	// (scope.EvOverlayChanged) and push tools/list_changed.
	TopicOverlay event.Topic = "session.overlay"
)

// Closed is the TopicClosed payload.
type Closed struct {
	Info   Info
	Reason CloseReason
}

// CloseReason distinguishes an explicit close from a TTL reap.
type CloseReason string

const (
	ReasonClosed  CloseReason = "closed"
	ReasonExpired CloseReason = "expired"
)

// OverlayChanged is the TopicOverlay payload.
type OverlayChanged struct {
	ID      SessionID
	Version uint64 // new overlay version
}

// Defaults for Options zero values (docs/architecture.md §7).
const (
	DefaultHTTPTTL      = 24 * time.Hour
	DefaultReapInterval = 5 * time.Minute
)

// Sentinel errors.
var (
	// ErrNotFound: no live session with that ID.
	ErrNotFound = errors.New("session: not found")
	// ErrLoosening: the mutation loosens a security field without the
	// human-grant flag (A.1 #8). The error text lists every violation.
	ErrLoosening = errors.New("session: overlay mutation loosens scope without human grant")
)

// SessionManager is the daemon-side session registry contract (docs/architecture.md §7
// ). Deviations from the sketch there, both deliberate:
//   - Mutate takes a ctx (the stdio path blocks on a gateway ack) and
//     variadic options carrying the human-grant flag (A.1 #8).
//   - Touch and Overlay are exposed for the HTTP bridge (LastSeen refresh)
//     and the scope resolver (Sources.Overlay) respectively.
type SessionManager interface {
	// Register admits a stdio gateway dialing in over the control socket.
	// link must be non-nil; the session lives until the link drops and the
	// daemon layer calls Close (stdio sessions are never TTL-reaped —
	// process lifetime IS session lifetime).
	Register(ctx context.Context, hello GatewayHello, link ControlLink) (*Session, error)
	// OpenHTTP mints a local HTTP session with a fresh 128-bit token.
	OpenHTTP(ctx context.Context, hello SessionHello) (*Session, error)
	Get(id SessionID) (*Session, bool)
	List() []Info
	// Mutate applies fn to a private deep copy of the session's overlay,
	// validates tighten-only (unless WithHumanGrant), then commits: HTTP
	// sessions swap in memory directly; stdio sessions first push the new
	// overlay over the ControlLink and commit only after the gateway acks
	// (authority in the daemon, execution in the gateway — a failed push
	// commits NOTHING, so daemon and gateway can never diverge).
	// On success the overlay version increments and TopicOverlay fires.
	Mutate(ctx context.Context, id SessionID, fn func(*scope.Overlay), opts ...MutateOption) error
	// Touch refreshes LastSeen (TTL liveness for HTTP sessions).
	Touch(id SessionID)
	// Overlay returns the session's current immutable overlay snapshot
	// (nil = none) — the scope.Sources.Overlay hook.
	Overlay(id SessionID) *scope.Overlay
	// Close removes the session, closes its link, and fires TopicClosed.
	// Idempotent; closing an unknown ID is a no-op.
	Close(id SessionID)
}

// MutateOption configures one Mutate call.
type MutateOption func(*mutateConfig)

type mutateConfig struct {
	humanGrant bool
}

// WithHumanGrant marks the mutation as human-approved, allowing it to
// LOOSEN the overlay (grant injection, --reset, restore beyond the agent's
// own narrowing). The approval flow that gates this flag is M1-C; callers
// must never pass it on an agent-reachable path.
func WithHumanGrant() MutateOption {
	return func(c *mutateConfig) { c.humanGrant = true }
}

// Options configures a MemoryManager. Zero values select the defaults.
type Options struct {
	// Bus receives lifecycle/overlay events; nil disables publishing.
	Bus *event.Bus
	// HTTPTTL is the idle TTL of HTTP sessions (default 24h).
	HTTPTTL time.Duration
	// ReapInterval is the reaper scan period for Run (default 5min).
	ReapInterval time.Duration
	// Clock overrides time.Now (tests).
	Clock func() time.Time
	// Rand is the token entropy source (default crypto/rand.Reader).
	Rand io.Reader
}

// MemoryManager is the in-memory SessionManager implementation. Overlays
// live only here — never on disk (A.1 #6); losing the process loses them.
type MemoryManager struct {
	bus          *event.Bus
	httpTTL      time.Duration
	reapInterval time.Duration
	clock        func() time.Time
	rand         io.Reader

	mu       sync.Mutex
	sessions map[SessionID]*Session
	seq      map[string]uint64 // clientID -> last assigned seq (monotonic, never reused)
}

var _ SessionManager = (*MemoryManager)(nil)

// NewMemoryManager builds a manager over opts.
func NewMemoryManager(opts Options) *MemoryManager {
	m := &MemoryManager{
		bus:          opts.Bus,
		httpTTL:      opts.HTTPTTL,
		reapInterval: opts.ReapInterval,
		clock:        opts.Clock,
		rand:         opts.Rand,
		sessions:     make(map[SessionID]*Session),
		seq:          make(map[string]uint64),
	}
	if m.httpTTL <= 0 {
		m.httpTTL = DefaultHTTPTTL
	}
	if m.reapInterval <= 0 {
		m.reapInterval = DefaultReapInterval
	}
	if m.clock == nil {
		m.clock = time.Now
	}
	if m.rand == nil {
		m.rand = rand.Reader
	}
	return m
}

// Register implements SessionManager.
func (m *MemoryManager) Register(ctx context.Context, hello GatewayHello, link ControlLink) (*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if hello.ClientID == "" {
		return nil, errors.New("session: empty client ID")
	}
	if link == nil {
		return nil, errors.New("session: stdio registration requires a control link")
	}
	s := m.mint(hello.ClientID, OriginStdioGateway, hello.Roots, hello.Caps)
	s.Link = link
	m.admit(s)
	return s, nil
}

// OpenHTTP implements SessionManager.
func (m *MemoryManager) OpenHTTP(ctx context.Context, hello SessionHello) (*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if hello.ClientID == "" {
		return nil, errors.New("session: empty client ID")
	}
	s := m.mint(hello.ClientID, OriginHTTP, hello.Roots, hello.Caps)
	if _, err := io.ReadFull(m.rand, s.token[:]); err != nil {
		// Fail-closed: a session without full-entropy token must not exist.
		return nil, fmt.Errorf("session: token entropy: %w", err)
	}
	m.admit(s)
	return s, nil
}

// mint builds a session with a fresh "client:seq" ID. Seq is per-client
// monotonic for the daemon's lifetime and never reused, so a re-registered
// gateway always gets a NEW identity (docs/architecture.md §7: overlay authority died
// with the old one; references must break, not silently rebind).
func (m *MemoryManager) mint(clientID string, origin Origin, roots []string, caps ClientCaps) *Session {
	now := m.clock()
	m.mu.Lock()
	m.seq[clientID]++
	n := m.seq[clientID]
	m.mu.Unlock()
	s := &Session{
		ID:        SessionID(fmt.Sprintf("%s:%d", clientID, n)),
		ClientID:  clientID,
		Seq:       n,
		Origin:    origin,
		Caps:      caps,
		StartedAt: now,
		roots:     cloneStrings(roots),
	}
	s.touch(now)
	return s
}

func (m *MemoryManager) admit(s *Session) {
	m.mu.Lock()
	m.sessions[s.ID] = s
	m.mu.Unlock()
	m.publish(event.Event{Topic: TopicOpened, Key: string(s.ID), Payload: s.info()})
}

// Get implements SessionManager.
func (m *MemoryManager) Get(id SessionID) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	return s, ok
}

// List implements SessionManager; ordered by ID for stable CLI output.
func (m *MemoryManager) List() []Info {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()
	out := make([]Info, len(sessions))
	for i, s := range sessions {
		out[i] = s.info()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Touch implements SessionManager.
func (m *MemoryManager) Touch(id SessionID) {
	if s, ok := m.Get(id); ok {
		s.touch(m.clock())
	}
}

// Overlay implements SessionManager.
func (m *MemoryManager) Overlay(id SessionID) *scope.Overlay {
	if s, ok := m.Get(id); ok {
		return s.Overlay()
	}
	return nil
}

// Mutate implements SessionManager. See the interface doc for the commit
// protocol; the per-session mutex makes concurrent Mutate calls fully
// serialized read-copy-update (no lost updates).
func (m *MemoryManager) Mutate(ctx context.Context, id SessionID, fn func(*scope.Overlay), opts ...MutateOption) error {
	var cfg mutateConfig
	for _, o := range opts {
		o(&cfg)
	}
	s, ok := m.Get(id)
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.overlay.Load()
	next := cloneOverlay(prev)
	fn(next)
	// Version is assigned HERE, after fn: the mutation fn cannot forge or
	// rewind versions, so the resolver cache key moves iff a commit happens.
	if prev != nil {
		next.Version = prev.Version + 1
	} else {
		next.Version = 1
	}

	if !cfg.humanGrant {
		if viols := loosenings(prev, next); len(viols) > 0 {
			// Fail-closed: reject the WHOLE mutation, not just the loosening
			// parts — partial application would commit a state nobody asked for.
			return fmt.Errorf("%w: %v", ErrLoosening, viols)
		}
	}

	if s.Origin == OriginStdioGateway {
		// Push-then-commit: the gateway executes the overlay, so the daemon
		// commits only what the gateway acked. On error nothing changes.
		if err := s.Link.PushOverlay(ctx, next); err != nil {
			return fmt.Errorf("session: push overlay to gateway: %w", err)
		}
	}
	s.overlay.Store(next)
	s.touch(m.clock())
	m.publish(event.Event{
		Topic:   TopicOverlay,
		Key:     string(id),
		Payload: OverlayChanged{ID: id, Version: next.Version},
	})
	return nil
}

// Close implements SessionManager.
func (m *MemoryManager) Close(id SessionID) {
	m.close(id, ReasonClosed)
}

func (m *MemoryManager) close(id SessionID, reason CloseReason) {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if ok {
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	if !ok {
		return
	}
	info := s.info()
	// Cascade: the overlay dies with the session (A.1 #6 — it never
	// existed anywhere else). Downstream cascades (confirm tokens, pending
	// grants, shaping cursors) key off the TopicClosed event.
	s.overlay.Store(nil)
	if s.Link != nil {
		_ = s.Link.Close() // best-effort; the link may already be gone
	}
	m.publish(event.Event{Topic: TopicClosed, Key: string(id), Payload: Closed{Info: info, Reason: reason}})
}

// Run drives the background reaper until ctx is done (docs/architecture.md §7: fixes
// toolport's mint/require-time-only retain). Safe to call once per manager.
func (m *MemoryManager) Run(ctx context.Context) {
	t := time.NewTicker(m.reapInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.reap(m.clock())
		}
	}
}

// reap closes HTTP sessions idle past the TTL. stdio sessions are exempt:
// their lifetime is the gateway process, cleaned up by link disconnect.
// Returns the number reaped (test hook).
func (m *MemoryManager) reap(now time.Time) int {
	m.mu.Lock()
	var expired []SessionID
	for id, s := range m.sessions {
		if s.Origin != OriginHTTP {
			continue
		}
		if now.Sub(s.LastSeen()) >= m.httpTTL {
			expired = append(expired, id)
		}
	}
	m.mu.Unlock()
	for _, id := range expired {
		m.close(id, ReasonExpired)
	}
	return len(expired)
}

// FindByToken resolves an HTTP session from its protocol-side token
// (Mcp-Session-Id), comparing in constant time per candidate. Failure
// direction: unknown or malformed token yields (nil, false) — deny.
func (m *MemoryManager) FindByToken(tokenHex string) (*Session, bool) {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()
	for _, s := range sessions {
		if s.MatchToken(tokenHex) {
			return s, true
		}
	}
	return nil, false
}

func (m *MemoryManager) publish(ev event.Event) {
	if m.bus != nil {
		m.bus.Publish(ev)
	}
}
