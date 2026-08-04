// Package oauthlogin runs an interactive OAuth login on behalf of a caller
// that has a browser when the process running the flow does not.
//
// WHY THIS EXISTS. `agenthub auth login` runs the whole dance in one
// foreground process: it discovers, opens a browser, blocks on a loopback
// callback and stores the result. A GUI cannot do that — it may not import
// internal/*, so it cannot reach oauthflow at all, and the control plane's
// request/response shape has nowhere to put a wait that lasts as long as a
// human takes to click "Approve".
//
// So the login is turned into a SESSION: it is started, it is polled, and it
// can be cancelled. What this package does NOT do is re-implement any part of
// the protocol — every login here goes through the same oauthflow.Flow the
// CLI drives, for the same reason internal/mcp is the only protocol facade.
// A second implementation of a security handshake is how the two drift and
// only one of them gets the fix.
//
// THE BROWSER IS THE CALLER'S JOB. LoginRequest.Open is wired to RECORD the
// authorization URL rather than open it. The daemon may be headless, may have
// been started by launchd with no session to draw into, and may not be the
// machine the user is sitting at; the process that asked for the login is the
// one that knows how to show a page. That inverts the CLI's arrangement and
// is the single behavioural difference between the two callers.
package oauthlogin

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/dinstein/agent-hub/internal/eventlog"
	"github.com/dinstein/agent-hub/internal/logx"
	"github.com/dinstein/agent-hub/internal/oauthflow"
)

// Phase is where one login session has got to.
type Phase string

const (
	// PhasePending: the flow is running. It may or may not have produced
	// something for the user to act on yet — that is AuthorizationURL and
	// UserCode, not this field.
	PhasePending Phase = "pending"
	// PhaseComplete: a credential was obtained and persisted.
	PhaseComplete Phase = "complete"
	// PhaseFailed: the flow returned an error, timed out, or was cancelled.
	PhaseFailed Phase = "failed"
)

// DefaultTTL bounds one login. It is deliberately generous: the wait is a
// human reading a consent screen, and a device code's own lifetime is
// commonly ten minutes.
const DefaultTTL = 10 * time.Minute

// DefaultRetain is how long a finished session stays readable. A poller that
// asks one moment after the flow ended must get the outcome, not a 404 that
// is indistinguishable from a session that never existed.
const DefaultRetain = 2 * time.Minute

// Request is one login to start.
type Request struct {
	// ServerID keys the vault entries. Required.
	ServerID string
	// Issuer pins the authorization server, skipping RFC 9728 discovery.
	Issuer string
	// ResourceURL is the MCP server being authorized against.
	ResourceURL string
	// ResourceMetadataURL is the pointer harvested from a 401.
	ResourceMetadataURL string
	// AuthorizationEndpoint replaces the discovered one, for providers that
	// serve an endpoint they never advertise.
	AuthorizationEndpoint string
	// Scopes is sent verbatim; empty means "whatever discovery found".
	Scopes []string
	// AllowLoopback permits an authorization server on this machine. It
	// follows the SERVER's own provenance, exactly as `auth login` does:
	// a local server's authorization endpoint may legitimately be loopback
	// too, and everything else stays SSRF-screened.
	AllowLoopback bool
	// CallbackPort re-binds the loopback port a previous login for this
	// server registered (0 = take a fresh one). Many providers match the
	// redirect_uri byte for byte, so a new random port is refused by exactly
	// the providers that were hardest to get working the first time.
	//
	// It is supplied by the caller rather than looked up here because the
	// caller already holds the credential store: this package would
	// otherwise need a second face onto the vault to read one integer.
	CallbackPort int
}

// Session is a snapshot of one login. It is returned BY VALUE: a caller must
// never hold a pointer into state a goroutine is still writing.
//
// It carries no token, no code and no device code. UserCode is the short
// string the human types into the provider's site and is meant to be shown;
// DeviceCode — the secret polled with — is not on this struct at all, so no
// rendering mistake can leak it.
type Session struct {
	ID     string
	Server string
	Phase  Phase
	// Mode is "" until the flow has picked one: it needs the authorization
	// server's metadata to decide, so the first poll of a session commonly
	// has nothing here yet.
	Mode string
	// AuthorizationURL is the page the caller must open (loopback mode).
	AuthorizationURL string
	// VerificationURI, VerificationURIComplete and UserCode are the device
	// flow's half.
	VerificationURI         string
	VerificationURIComplete string
	UserCode                string
	// Deadline is when this session gives up on its own.
	Deadline time.Time
	// Issuer, Scope, ExpiresAt and HasRefreshToken describe what was stored,
	// and are set only once Phase is PhaseComplete.
	Issuer          string
	Scope           string
	ExpiresAt       int64
	HasRefreshToken bool
	// Err is why the login failed, set only once Phase is PhaseFailed.
	Err error
}

// Actionable reports that this session is waiting on the human and has told
// the caller how to reach them. A pending session that is not yet actionable
// is still discovering, and there is nothing to show but a spinner.
func (s Session) Actionable() bool {
	return s.Phase == PhasePending && (s.AuthorizationURL != "" || s.UserCode != "")
}

// Flow is the part of *oauthflow.Flow this package drives.
//
// It is an interface so that what this package actually OWNS — session ids,
// phases, retention, cancellation, the one-login-per-server rule — can be
// tested without standing up an authorization server. The protocol those
// rules wrap is not this package's subject and has its own suite next door
// in internal/oauthflow; a test here that needed a fake AS would be testing
// that suite again, badly.
type Flow interface {
	Login(ctx context.Context, req oauthflow.LoginRequest) (*oauthflow.LoginResult, error)
}

// FlowFactory builds the flow for one login.
//
// It is a factory rather than a single shared Flow because AllowLoopback is
// baked into the oauthflow.Client's SSRF screen at construction time, and it
// varies per server. Hoisting it out would mean either screening every login
// against the loosest server's rule or rebuilding the screen behind the
// client's back.
type FlowFactory func(allowLoopback bool) Flow

// Config assembles a Manager.
type Config struct {
	// Flows is required.
	Flows FlowFactory
	// TTL bounds one login (0 = DefaultTTL).
	TTL time.Duration
	// Retain is how long a finished session stays readable (0 = DefaultRetain).
	Retain time.Duration
	// Now overrides time.Now (tests).
	Now func() time.Time

	// Events and Log receive one record per phase of every login: started,
	// waiting on the human, then completed or failed.
	//
	// Before them a login was invisible from outside this process. The one
	// step of setting up a downstream that BLOCKS on a person — waiting for a
	// consent screen somebody may never have seen — left nothing in the
	// timeline at all, so "it has been pending for ten minutes" and "it
	// failed at discovery a second in" read identically from the outside.
	//
	// Both may be nil; a nil stream still writes prose and a nil logger still
	// writes records (eventlog.Emit).
	Events *eventlog.Stream
	Log    *slog.Logger
}

// Manager owns the live login sessions.
type Manager struct {
	flows  FlowFactory
	ttl    time.Duration
	retain time.Duration
	now    func() time.Time
	events *eventlog.Stream
	log    *slog.Logger

	mu       sync.Mutex
	sessions map[string]*session
}

// session is the mutable half. Every field behind Manager.mu.
type session struct {
	snap   Session
	cancel context.CancelFunc
	// ended is when the flow finished, and is what Retain is measured from.
	ended time.Time
}

// New builds a Manager. A nil Flows factory is a programming error and is
// reported at construction rather than on the first login.
func New(cfg Config) (*Manager, error) {
	if cfg.Flows == nil {
		return nil, errors.New("oauthlogin: Config.Flows is required")
	}
	m := &Manager{
		flows:    cfg.Flows,
		ttl:      cfg.TTL,
		retain:   cfg.Retain,
		now:      cfg.Now,
		events:   cfg.Events,
		log:      cfg.Log,
		sessions: map[string]*session{},
	}
	if m.ttl <= 0 {
		m.ttl = DefaultTTL
	}
	if m.retain <= 0 {
		m.retain = DefaultRetain
	}
	if m.now == nil {
		m.now = time.Now
	}
	return m, nil
}

// ErrNoSession reports an id that names no live or recently finished session.
var ErrNoSession = errors.New("oauthlogin: no such login session")

// Start begins a login and returns its first snapshot immediately.
//
// It takes no context on purpose: the flow outlives the call that asked for
// it. Binding it to a request context would cancel the login the moment the
// HTTP handler returned, which is the one thing this whole package exists to
// avoid.
//
// A server that already has a live login gets THAT session back instead of a
// second one. Two concurrent flows for the same server would each bind their
// own loopback port and race to write the same vault entry, and the losing
// one leaves the user staring at a consent screen whose callback goes
// nowhere. A double-clicked button must not be able to arrange that.
func (m *Manager) Start(req Request) (Session, error) {
	if req.ServerID == "" {
		return Session{}, errors.New("oauthlogin: login needs a server id")
	}
	if req.ResourceURL == "" && req.Issuer == "" {
		return Session{}, fmt.Errorf("oauthlogin: %s has no url to authorize against", req.ServerID)
	}
	id, err := newSessionID()
	if err != nil {
		return Session{}, err
	}

	m.mu.Lock()
	m.sweepLocked()
	for _, s := range m.sessions {
		if s.snap.Server == req.ServerID && s.snap.Phase == PhasePending {
			snap := s.snap
			m.mu.Unlock()
			return snap, nil
		}
	}
	now := m.now()
	ctx, cancel := context.WithDeadline(context.Background(), now.Add(m.ttl))
	s := &session{
		snap: Session{
			ID:       id,
			Server:   req.ServerID,
			Phase:    PhasePending,
			Deadline: now.Add(m.ttl),
		},
		cancel: cancel,
	}
	m.sessions[id] = s
	snap := s.snap
	m.mu.Unlock()

	m.emit(snap, eventlog.KindOAuthLoginStarted, "interactive login started", "")
	go m.run(ctx, cancel, id, req)
	return snap, nil
}

// Get returns a snapshot of one session.
func (m *Manager) Get(id string) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepLocked()
	s, ok := m.sessions[id]
	if !ok {
		return Session{}, ErrNoSession
	}
	return s.snap, nil
}

// Cancel stops a running login and returns its final snapshot.
//
// Cancelling a login that has already finished is not an error: it is what a
// user clicking "Cancel" at the same moment the callback lands does, and
// answering that with a failure would report a stored credential as a
// cancelled one.
func (m *Manager) Cancel(id string) (Session, error) {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return Session{}, ErrNoSession
	}
	cancel, running := s.cancel, s.snap.Phase == PhasePending
	m.mu.Unlock()

	if running && cancel != nil {
		cancel()
	}
	// The goroutine writes the terminal snapshot; wait for it briefly so the
	// answer describes the session as it now is rather than as it was.
	for range 100 {
		snap, err := m.Get(id)
		if err != nil || snap.Phase != PhasePending {
			return snap, err
		}
		time.Sleep(2 * time.Millisecond)
	}
	return m.Get(id)
}

// run drives one login to its end.
func (m *Manager) run(ctx context.Context, cancel context.CancelFunc, id string, req Request) {
	defer cancel()
	flow := m.flows(req.AllowLoopback)

	lreq := oauthflow.LoginRequest{
		ServerID:              req.ServerID,
		Issuer:                req.Issuer,
		ResourceURL:           req.ResourceURL,
		ResourceMetadataURL:   req.ResourceMetadataURL,
		AuthorizationEndpoint: req.AuthorizationEndpoint,
		Scopes:                req.Scopes,
		ClientName:            "agenthub",
		// Open records the URL instead of opening it (see the package
		// comment). Returning nil matters: a non-nil error is oauthflow's
		// documented signal that this host has no browser, which downgrades
		// the flow to the manual paste path — and there is nobody on this
		// API to paste anything.
		Open: func(u string) error {
			snap := m.update(id, func(s *Session) {
				s.Mode = string(oauthflow.ModeLoopback)
				s.AuthorizationURL = u
			})
			// The URL is not a secret — it is the page the human is being
			// asked to open, and the code exchanged on the way back never
			// travels in it.
			m.emit(snap, eventlog.KindOAuthLoginWaiting, "login is waiting for the browser", u)
			return nil
		},
		OnDeviceCode: func(da oauthflow.DeviceAuthorization) {
			snap := m.update(id, func(s *Session) {
				s.Mode = string(oauthflow.ModeDevice)
				s.VerificationURI = da.VerificationURI
				s.VerificationURIComplete = da.VerificationURIComplete
				s.UserCode = da.UserCode
			})
			// The verification URI, never the device code: UserCode is shown
			// to the human but DeviceCode is the secret polled with, and a
			// record is a file somebody else may read.
			m.emit(snap, eventlog.KindOAuthLoginWaiting, "login is waiting for a device code",
				da.VerificationURI)
		},
		// Paste stays nil. Manual mode reads a pasted callback URL from a
		// terminal, so SelectMode must never choose it here — and with Open
		// non-nil it cannot. Leaving it nil ALSO disables oauthflow's
		// loopback-to-manual downgrade, which would otherwise leave the flow
		// blocked forever on a paste this API has no way to deliver.
	}
	lreq.FixedCallbackPort = req.CallbackPort

	res, err := flow.Login(ctx, lreq)
	snap := m.update(id, func(s *Session) {
		if err != nil {
			s.Phase = PhaseFailed
			s.Err = err
			return
		}
		s.Phase = PhaseComplete
		s.Mode = string(res.Mode)
		if st := res.State; st != nil {
			s.Issuer = st.Issuer
			s.Scope = st.Scope
			s.ExpiresAt = st.ExpiresAt
			s.HasRefreshToken = st.RefreshToken != ""
		}
	})
	m.mu.Lock()
	if s, ok := m.sessions[id]; ok && s.snap.Phase != PhasePending {
		s.ended = m.now()
	}
	m.mu.Unlock()

	m.logDiscovery(snap, res, err)
	if err != nil {
		m.emit(snap, eventlog.KindOAuthLoginFailed, "interactive login failed", err.Error())
		return
	}
	m.emit(snap, eventlog.KindOAuthLoginCompleted, "interactive login completed", snap.Mode)
}

// logDiscovery writes how the metadata chain went, at Debug.
//
// oauthflow is a leaf with no logging dependency: it reports what happened as
// data (DiscoveryResult.Attempted, FlowError.Attempted) and leaves rendering
// to whoever holds a logger. This is that end of the arrangement, and until
// it existed the data was collected and thrown away — the chain deciding
// which endpoints an entire login talks to left no trace, on the path where
// "this provider will not connect" is most often reported.
//
// BOTH outcomes, not only the failure. A login that succeeded through the
// synthesized-endpoints fallback is one candidate away from one that failed,
// and it is the case that goes wrong later: a 403 from a guessed /register
// means something entirely different from a 403 from an advertised
// registration_endpoint, which is the distinction DiscoveryDefaults exists
// to record.
//
// URLs only. These are metadata endpoints — the class the flow's own error
// strings already interpolate — and no token, code or client secret ever
// passes through a DiscoveryResult.
func (m *Manager) logDiscovery(snap Session, res *oauthflow.LoginResult, loginErr error) {
	if m.log == nil {
		return
	}
	var (
		status   oauthflow.DiscoveryStatus
		attempts []oauthflow.Attempt
		fe       *oauthflow.FlowError
	)
	switch {
	// The STATUS, not merely the error's Go type, is what says this failure
	// came out of the chain. FlowError is what nearly everything in oauthflow
	// returns — a browser that would not open, a token exchange refused — and
	// every branch that is not about discovery leaves Discovery at its zero
	// value. Matching on the type alone therefore described a walk that never
	// happened, as `status="" candidates=0`: the empty chain the default case
	// below exists to avoid, reached through the door beside it.
	//
	// A set status with no attempts is a different thing and still logged.
	// DiscoveryProtected on a document that lists no authorization_servers
	// says where the walk stopped, which is the answer even when no candidate
	// line follows it.
	case loginErr != nil && errors.As(loginErr, &fe) && fe.Discovery != "":
		status, attempts = fe.Discovery, fe.Attempted
	case res != nil && res.Discovery != nil:
		status, attempts = res.Discovery.Status, res.Discovery.Attempted
	default:
		// A failure that never reached discovery, or a result carrying none.
		// There is nothing to describe, and an empty chain would read as
		// "every candidate was fine".
		return
	}
	m.log.Debug("oauth discovery finished", logx.Server(snap.Server), logx.Session(snap.ID),
		"status", string(status), "candidates", len(attempts))
	for _, a := range attempts {
		m.log.Debug("oauth discovery candidate", logx.Server(snap.Server), logx.Session(snap.ID),
			"url", a.URL, "outcome", a.Outcome)
	}
}

// emit writes one login phase to both streams. The session id travels in the
// record's Session field, so the four records of one login join to each other
// and to nothing else — a server may be logged in to twice in a day.
func (m *Manager) emit(snap Session, kind eventlog.Kind, msg, detail string) {
	if snap.Server == "" {
		return
	}
	m.events.Emit(m.log, eventlog.Record{
		Scope: eventlog.ScopeServer, Kind: kind,
		Server: snap.Server, Session: snap.ID, Detail: detail,
	}, msg, logx.Server(snap.Server), logx.Session(snap.ID), "mode", snap.Mode)
}

// update mutates one session's snapshot under the lock. A session that has
// already been swept is a no-op rather than a panic: the flow's callbacks are
// still running at that point and they do not get to decide the lifetime.
// It returns the resulting snapshot BY VALUE, which is what the caller then
// records: reading the session again outside the lock would race the flow's
// own callbacks.
func (m *Manager) update(id string, fn func(*Session)) Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return Session{}
	}
	fn(&s.snap)
	return s.snap
}

// sweepLocked drops sessions nobody can act on any more: finished ones past
// their retention, and pending ones past their deadline whose goroutine has
// not yet noticed. Called from the read and write paths rather than from a
// background ticker, so the Manager owns no goroutine of its own and needs
// no shutdown.
func (m *Manager) sweepLocked() {
	now := m.now()
	for id, s := range m.sessions {
		switch {
		case s.snap.Phase != PhasePending && !s.ended.IsZero() && now.Sub(s.ended) > m.retain:
			delete(m.sessions, id)
		case s.snap.Phase == PhasePending && now.After(s.Deadline().Add(m.retain)):
			if s.cancel != nil {
				s.cancel()
			}
			delete(m.sessions, id)
		}
	}
}

// Deadline is the session's own give-up time.
func (s *session) Deadline() time.Time { return s.snap.Deadline }

// newSessionID mints an unguessable handle.
//
// Failure direction: a crypto/rand failure returns an error and no session,
// with no fallback to a counter or a timestamp. The id is what separates one
// caller's login from another's on a shared control plane, and polling with a
// guessed one would hand over an authorization URL that is mid-PKCE.
func newSessionID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("oauthlogin: entropy unavailable: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
