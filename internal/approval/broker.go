package approval

import (
	"context"
	"crypto/rand"
	"fmt"
	"slices"
	"sync"
	"time"
)

// LiveCheck reports whether (server, tool, fingerprint) still matches the
// live router definition at answer time. It is injected by the daemon
// assembly (Stage 2 wires router.Catalog). Fail direction: returning false
// turns an approval into Stale — the call is rejected and must be re-gated
// against the current definition. A nil LiveCheck disables the answer-time
// re-check only; the pipeline's independent args_hash recomputation before
// execution (docs/flows.md) still stands.
type LiveCheck func(server, tool, fingerprint string) bool

// Options tunes a MemBroker. The zero value is usable (no allowlist, no
// live check, 120s TTL).
type Options struct {
	// Allowlist consulted before fanning out and written by
	// remember-forever answers. nil = every gated call goes to a human.
	Allowlist *Allowlist
	// Check is the answer-time drift re-check. See LiveCheck.
	Check LiveCheck
	// DefaultTTL stamps Deadline on requests that arrive without one
	// (default 120s, docs/flows.md).
	DefaultTTL time.Duration
	// Retention keeps terminal requests around so late answers get typed
	// ErrExpired/ErrAlreadyDecided instead of ErrUnknownToken (default 10m).
	Retention time.Duration
	// SubscriberBuffer is the per-frontend channel depth (default 16).
	SubscriberBuffer int
	// Now overrides the clock (tests). Default time.Now. The Ask wait timer
	// always uses the real clock; Now covers stamping and expiry checks.
	Now func() time.Time
}

const (
	defaultTTL       = 120 * time.Second
	defaultRetention = 10 * time.Minute
	defaultSubBuffer = 16
	pruneEvery       = time.Minute
)

// MemBroker is the in-memory Broker implementation living inside the daemon.
// It is safe for concurrent use.
type MemBroker struct {
	opts Options

	mu            sync.Mutex
	subs          map[int]chan Request
	nextSub       int
	pending       map[string]*pendingReq
	sessionGrants map[string]map[string]Entry // sessionID -> fingerprint -> grant
	lastPrune     time.Time
}

// pendingReq is one in-flight request. terminal is written exactly once
// under MemBroker.mu (first decision wins); done is buffered(1) and receives
// that single terminal value for the blocked Ask. decidedBy is the frontend
// identity of a HUMAN decision ("" for broker-internal terminals such as
// Timedout/Unreachable).
type pendingReq struct {
	req       Request
	done      chan Decision
	terminal  *Decision
	decidedAt time.Time
	decidedBy string
}

var (
	_ Broker = (*MemBroker)(nil)
	_ Asker  = (*MemBroker)(nil)
)

// NewMemBroker builds a broker. See Options for defaults.
func NewMemBroker(opts Options) *MemBroker {
	if opts.DefaultTTL <= 0 {
		opts.DefaultTTL = defaultTTL
	}
	if opts.Retention <= 0 {
		opts.Retention = defaultRetention
	}
	if opts.SubscriberBuffer <= 0 {
		opts.SubscriberBuffer = defaultSubBuffer
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &MemBroker{
		opts:          opts,
		subs:          map[int]chan Request{},
		pending:       map[string]*pendingReq{},
		sessionGrants: map[string]map[string]Entry{},
	}
}

// Ask implements Broker. It first consults remembered grants (session, then
// allowlist); on a hit it returns Approved without involving a human. It
// then fails Unreachable when no frontend is subscribed, otherwise fans the
// request out and blocks until the first Answer, the deadline (Timedout), or
// ctx cancellation (the asker vanished — recorded as Unreachable so a late
// human answer gets ErrExpired, never an execution).
func (b *MemBroker) Ask(ctx context.Context, req Request) Decision {
	if b.allowHit(req) {
		return Approved
	}

	b.mu.Lock()
	b.pruneLocked()
	if len(b.subs) == 0 {
		b.mu.Unlock()
		// Fail-closed: nobody can possibly approve, so reject now instead
		// of stranding the agent until the deadline.
		return Unreachable
	}
	if req.Token == "" {
		req.Token = rand.Text()
	}
	if req.Deadline.IsZero() {
		// Broker stamps the deadline so the UI countdown and the auto-deny
		// fire at the same instant.
		req.Deadline = b.opts.Now().Add(b.opts.DefaultTTL)
	}
	p := &pendingReq{req: req, done: make(chan Decision, 1)}
	b.pending[req.Token] = p
	// Fan out under the lock: sends are non-blocking, and holding mu here
	// lets Subscribe's cancel close channels without racing a send. A full
	// (slow) frontend is skipped — dropping degrades towards Timedout,
	// never towards blocking the data plane or approving anything.
	for _, ch := range b.subs {
		select {
		case ch <- req:
		default:
		}
	}
	b.mu.Unlock()

	timer := time.NewTimer(time.Until(req.Deadline))
	defer timer.Stop()
	select {
	case d := <-p.done:
		return d
	case <-timer.C:
		return b.finish(req.Token, Timedout)
	case <-ctx.Done():
		return b.finish(req.Token, Unreachable)
	}
}

// finish records d as the terminal decision unless one already exists, and
// returns whichever decision actually stands (an Answer racing the deadline
// wins if it got there first).
func (b *MemBroker) finish(token string, d Decision) Decision {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.pending[token]
	if !ok {
		return d
	}
	if p.terminal != nil {
		return *p.terminal
	}
	b.setTerminalLocked(p, d)
	return d
}

// setTerminalLocked marks p decided. Caller holds b.mu and has verified
// p.terminal == nil, so the buffered send can never block.
func (b *MemBroker) setTerminalLocked(p *pendingReq, d Decision) {
	t := d
	p.terminal = &t
	p.decidedAt = b.opts.Now()
	p.done <- d
}

// Subscribe implements Broker. Pending undecided requests are replayed into
// the new channel so a late-attaching frontend sees the whole queue. The
// returned cancel is idempotent and closes the channel.
func (b *MemBroker) Subscribe(frontend string) (<-chan Request, func()) {
	_ = frontend // display-only for now; kept for the ctlapi SSE bridge
	b.mu.Lock()
	ch := make(chan Request, b.opts.SubscriberBuffer)
	id := b.nextSub
	b.nextSub++
	b.subs[id] = ch
	for _, p := range b.pending {
		if p.terminal == nil {
			select {
			case ch <- p.req:
			default:
			}
		}
	}
	b.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, id)
			// Safe to close under mu: every send targets b.subs under mu.
			close(ch)
			b.mu.Unlock()
		})
	}
	return ch, cancel
}

// FrontendCount reports the number of live subscribers (docs/flows.md:
// FrontendCount()==0 → Unreachable).
func (b *MemBroker) FrontendCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

// Answer implements Broker. First answer wins. On approve, the injected
// LiveCheck runs first: if the tool definition drifted the waiter receives
// Stale (not Approved) and Answer returns ErrStale — no remember grant is
// recorded for a stale approval. Remember-grant failures come back wrapped
// in ErrRememberFailed while the single approval itself stands.
func (b *MemBroker) Answer(token string, approve bool, remember RememberScope) error {
	return b.AnswerAs(token, approve, remember, "")
}

// AnswerAs is Answer with the deciding frontend's identity attached ("cli",
// "gui", ...). The identity is display/audit metadata only — it never
// influences the decision — and is reported back to late frontends through
// Requests and the ErrAlreadyDecided message (docs/modules/controlplane.md: the 409
// carries the first decider).
func (b *MemBroker) AnswerAs(token string, approve bool, remember RememberScope, by string) error {
	b.mu.Lock()
	b.pruneLocked()
	p, ok := b.pending[token]
	if !ok {
		b.mu.Unlock()
		return fmt.Errorf("answer %q: %w", token, ErrUnknownToken)
	}
	if p.terminal != nil {
		return b.decidedErrLocked(token, p)
	}
	if b.opts.Now().After(p.req.Deadline) {
		// The deadline passed but Ask's timer has not fired yet. Record the
		// timeout here: late approvals never execute.
		b.setTerminalLocked(p, Timedout)
		b.mu.Unlock()
		return fmt.Errorf("answer %q: %w", token, ErrExpired)
	}
	if !approve {
		b.setTerminalLocked(p, Denied)
		p.decidedBy = by
		b.mu.Unlock()
		return nil
	}

	// Approve path. Run the live-definition re-check outside the lock — it
	// may consult the router — then re-validate that nothing raced us.
	req := p.req
	check := b.opts.Check
	b.mu.Unlock()
	live := check == nil || check(req.Server, req.Tool, req.Fingerprint)

	b.mu.Lock()
	if p.terminal != nil {
		return b.decidedErrLocked(token, p)
	}
	if !live {
		b.setTerminalLocked(p, Stale)
		p.decidedBy = by
		b.mu.Unlock()
		return fmt.Errorf("answer %q %s/%s: %w", token, req.Server, req.Tool, ErrStale)
	}
	b.setTerminalLocked(p, Approved)
	p.decidedBy = by
	var rememberErr error
	if remember == RememberSession {
		rememberErr = b.rememberSessionLocked(req)
	}
	b.mu.Unlock()

	if remember == RememberForever {
		rememberErr = b.rememberForever(req)
	}
	return rememberErr
}

// decidedErrLocked maps an existing terminal state to the typed answer
// error and releases the lock. The first decider's identity rides along in
// the ErrAlreadyDecided message (docs/modules/controlplane.md).
func (b *MemBroker) decidedErrLocked(token string, p *pendingReq) error {
	d := *p.terminal
	by := p.decidedBy
	b.mu.Unlock()
	if d == Timedout || d == Unreachable {
		return fmt.Errorf("answer %q: %w", token, ErrExpired)
	}
	if by == "" {
		return fmt.Errorf("answer %q (decision %s): %w", token, d, ErrAlreadyDecided)
	}
	return fmt.Errorf("answer %q (decision %s by %s): %w", token, d, by, ErrAlreadyDecided)
}

// rememberSessionLocked records an in-memory session grant. Caller holds mu.
func (b *MemBroker) rememberSessionLocked(req Request) error {
	if req.Fingerprint == "" {
		return fmt.Errorf("%w: request has no tool fingerprint", ErrRememberFailed)
	}
	if req.SessionID == "" {
		return fmt.Errorf("%w: request has no session id", ErrRememberFailed)
	}
	g := b.sessionGrants[req.SessionID]
	if g == nil {
		g = map[string]Entry{}
		b.sessionGrants[req.SessionID] = g
	}
	g[req.Fingerprint] = grantEntry(req, b.opts.Now())
	return nil
}

// rememberForever writes the fingerprint-bound allowlist entry.
func (b *MemBroker) rememberForever(req Request) error {
	if req.Fingerprint == "" {
		return fmt.Errorf("%w: request has no tool fingerprint", ErrRememberFailed)
	}
	if b.opts.Allowlist == nil {
		return fmt.Errorf("%w: no allowlist configured", ErrRememberFailed)
	}
	if err := b.opts.Allowlist.Add(grantEntry(req, b.opts.Now())); err != nil {
		return fmt.Errorf("%w: %w", ErrRememberFailed, err)
	}
	return nil
}

// grantEntry builds the remembered grant for req: fingerprint keyed, bound
// to server+tool (defense in depth), NOT bound to ArgsHash — remembering
// means "this exact tool definition, any future arguments" (docs/flows.md:
// allowlist key = server/tool/fingerprint). No argument bytes are stored.
func grantEntry(req Request, now time.Time) Entry {
	return Entry{
		Fingerprint: req.Fingerprint,
		Server:      req.Server,
		Tool:        req.Tool,
		GateReason:  req.GateReason,
		CreatedAt:   now,
	}
}

// ForgetSession drops all session-scoped grants for sessionID (hook for the
// session reaper; grants must not outlive the session they were scoped to).
func (b *MemBroker) ForgetSession(sessionID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.sessionGrants, sessionID)
}

// allowHit consults session grants then the allowlist. Fail direction: an
// empty fingerprint never hits — unfingerprinted calls always go to a human.
func (b *MemBroker) allowHit(req Request) bool {
	if req.Fingerprint == "" {
		return false
	}
	if req.SessionID != "" {
		b.mu.Lock()
		if g := b.sessionGrants[req.SessionID]; g != nil {
			if e, ok := g[req.Fingerprint]; ok && e.matches(req) {
				b.mu.Unlock()
				return true
			}
		}
		b.mu.Unlock()
	}
	return b.opts.Allowlist.Match(req)
}

// RequestStatus is the listing view of one tracked request (`agenthub
// approval ls`). Decision == nil means the request is still pending.
//
// The embedded Request keeps its ArgsJSON — it stays memory/socket-only:
// consumers (ctlapi) must never persist it and must strip it from any
// response that is not the authenticated SSE/control channel.
type RequestStatus struct {
	Request   Request
	Decision  *Decision // nil = pending
	DecidedAt time.Time
	DecidedBy string
}

// Requests returns a snapshot of every tracked request: pending ones plus
// terminal ones still inside the Retention window (the `--history` view).
// Order: pending first by deadline, then decided by decision time (newest
// first).
func (b *MemBroker) Requests() []RequestStatus {
	b.mu.Lock()
	b.pruneLocked()
	out := make([]RequestStatus, 0, len(b.pending))
	for _, p := range b.pending {
		st := RequestStatus{Request: p.req, DecidedAt: p.decidedAt, DecidedBy: p.decidedBy}
		if p.terminal != nil {
			d := *p.terminal
			st.Decision = &d
		}
		out = append(out, st)
	}
	b.mu.Unlock()
	slices.SortFunc(out, func(a, b RequestStatus) int {
		ap, bp := a.Decision == nil, b.Decision == nil
		if ap != bp {
			if ap {
				return -1 // pending before decided
			}
			return 1
		}
		if ap { // both pending: earlier deadline first
			return a.Request.Deadline.Compare(b.Request.Deadline)
		}
		return b.DecidedAt.Compare(a.DecidedAt) // both decided: more recent first
	})
	return out
}

// pruneLocked evicts terminal requests older than Retention. Caller holds
// mu. Rate-limited so the sweep cost stays off the hot path.
func (b *MemBroker) pruneLocked() {
	now := b.opts.Now()
	if now.Sub(b.lastPrune) < pruneEvery {
		return
	}
	b.lastPrune = now
	for tok, p := range b.pending {
		if p.terminal != nil && now.Sub(p.decidedAt) > b.opts.Retention {
			delete(b.pending, tok)
		}
	}
}
