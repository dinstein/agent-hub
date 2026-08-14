package session

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/dinstein/agent-hub/internal/event"
)

// Bus topics published by the manager. Payload types are documented per
// topic; all payloads are immutable snapshots.
const (
	// TopicOpened fires on Register/OpenHTTP. Key = session ID; payload Info.
	TopicOpened event.Topic = "session.opened"
	// TopicClosed fires on Close (explicit or reaped). Key = session ID;
	// payload Closed.
	TopicClosed event.Topic = "session.closed"
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

// Defaults for Options zero values (docs/model.md).
const (
	DefaultHTTPTTL      = 24 * time.Hour
	DefaultReapInterval = 5 * time.Minute
)

// Sentinel errors.
var (
	// ErrNotFound: no live session with that ID.
	ErrNotFound = errors.New("session: not found")
)

// SessionManager is the daemon-side session registry contract
// (docs/model.md). Deviations from the sketch there, both
// deliberate:
//   - Touch is exposed for the HTTP bridge (LastSeen refresh).
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
	// Touch refreshes LastSeen (TTL liveness for HTTP sessions).
	Touch(id SessionID)
	// Close removes the session, closes its link, and fires TopicClosed.
	// Idempotent; closing an unknown ID is a no-op.
	Close(id SessionID)
}

// Options configures a MemoryManager. Zero values select the defaults.
type Options struct {
	// Bus receives session lifecycle events (opened, closed); nil disables
	// publishing.
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

// MemoryManager is the in-memory SessionManager implementation. A session
// lives only here — never on disk; losing the process loses it.
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
// gateway always gets a NEW identity (docs/model.md): a reference
// held to the old session must break rather than silently rebind to the new
// one, which is what reusing a seq would make it do.
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
	slices.SortFunc(out, func(a, b Info) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

// Touch implements SessionManager.
func (m *MemoryManager) Touch(id SessionID) {
	if s, ok := m.Get(id); ok {
		s.touch(m.clock())
	}
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
	// Downstream cascades (shaping cursors and anything else keyed on a live
	// session) key off the TopicClosed event.
	if s.Link != nil {
		_ = s.Link.Close() // best-effort; the link may already be gone
	}
	m.publish(event.Event{Topic: TopicClosed, Key: string(id), Payload: Closed{Info: info, Reason: reason}})
}

// Run drives the background reaper until ctx is done (docs/model.md: fixes
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
