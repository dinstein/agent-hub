package gateway

import (
	"context"
	"sync"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// Subscriptions are the gateway→client direction on the in-process face.
//
// The stdio face writes a notification straight to the client's stdout, and
// has since the beginning. The in-process face has many clients behind one
// Conn — one per credential, shared by every HTTP session that credential
// opened — so "write it to the client" is not a thing this file can do. It
// hands each consumer its own Subscription instead, and internal/httpbridge
// turns one of those into an SSE stream.
//
// Nothing here decides WHAT may be sent: a notification reaching fanout was
// already produced by the gateway body for this credential's scope, so the
// filter below is the client's own request narrowing what it wants, never a
// security boundary. The gate chain sits in front of the pipeline, and no
// part of it is re-implemented on this path.

// Subscription is one consumer of a Conn's notification direction.
//
// Delivery is COALESCING, latest-wins per method, and that is a deliberate
// choice over a buffered channel. Every notification this face carries is an
// edge with no payload the client needs — `tools/list_changed` says "re-list",
// not what changed — so collapsing two of them into one costs the client
// nothing. A fixed buffer would have to answer a harder question when it
// fills: dropping the NEWEST edge is how a client ends up believing a stale
// tool set with nothing saying so, which is the failure this whole face
// exists to remove. Coalescing never drops the newest, never blocks the read
// loop, and is bounded by the number of distinct methods rather than by the
// rate they arrive at.
type Subscription struct {
	c *Conn
	// accept narrows what this consumer wants. nil accepts everything,
	// which is the ≤ 2025-11-25 GET stream's shape; the 2026-07-28
	// subscriptions/listen filter is an allow list the server must honour
	// exactly, and arrives here already compiled into this predicate.
	accept func(method string) bool

	mu sync.Mutex
	// pending holds the latest notification per method, and order holds the
	// methods in arrival order, so coalescing cannot reorder two DIFFERENT
	// methods relative to each other.
	pending map[string]*mcp.Notification
	order   []string
	closed  bool

	// signal is capacity 1 and carries no value: it wakes Next, which reads
	// the state above rather than the channel. A full signal channel means
	// "already awake", so offer never blocks on a consumer that is slow.
	signal    chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

// Subscribe returns a new consumer of this connection's notification
// direction. accept may be nil, meaning every method.
//
// The caller MUST Close the subscription; an abandoned one holds its slot in
// the Conn for the life of the connection. Bounding how many may exist is the
// caller's job — internal/httpbridge holds that quota, because the resource
// actually being limited is an open HTTP response, not a map entry here.
func (c *Conn) Subscribe(accept func(method string) bool) (*Subscription, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrConnClosed
	}
	s := &Subscription{
		c:       c,
		accept:  accept,
		pending: make(map[string]*mcp.Notification),
		signal:  make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
	if c.subs == nil {
		c.subs = make(map[*Subscription]struct{})
	}
	c.subs[s] = struct{}{}
	return s, nil
}

// Next returns the next notification, blocking until one arrives.
//
// ok=false means no further notification will ever arrive on this
// subscription: ctx ended, or the subscription (or its Conn) closed. Whatever
// had already been coalesced is handed over FIRST — a closing stream flushes
// what it holds rather than discarding it, since the client is usually still
// reading the response body while the gateway tears down.
func (s *Subscription) Next(ctx context.Context) (*mcp.Notification, bool) {
	for {
		s.mu.Lock()
		if len(s.order) > 0 {
			method := s.order[0]
			s.order = s.order[1:]
			n := s.pending[method]
			delete(s.pending, method)
			s.mu.Unlock()
			return n, true
		}
		closed := s.closed
		s.mu.Unlock()
		if closed {
			return nil, false
		}
		select {
		case <-ctx.Done():
			return nil, false
		case <-s.done:
			// Loop rather than return: anything offered between the check
			// above and this wake-up is still owed to the caller, and the
			// next pass returns it before reporting the close.
		case <-s.signal:
		}
	}
}

// Close releases the subscription. Idempotent, and safe to call while another
// goroutine is blocked in Next — that call returns ok=false once it has
// handed over whatever was already pending.
func (s *Subscription) Close() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		close(s.done)
		s.c.dropSubscription(s)
	})
}

// offer hands one notification to this subscription. It NEVER blocks: the
// read loop it runs on is the only reader of the gateway's output pipe, and
// a consumer that stopped reading must not be able to stall the connection
// every other session on this credential shares.
func (s *Subscription) offer(n *mcp.Notification) {
	if s.accept != nil && !s.accept(n.Method) {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if _, queued := s.pending[n.Method]; !queued {
		s.order = append(s.order, n.Method)
	}
	s.pending[n.Method] = n
	s.mu.Unlock()

	select {
	case s.signal <- struct{}{}:
	default: // already awake
	}
}

// fanout offers one gateway notification to every live subscription.
//
// With nobody subscribed this is the old behaviour, and deliberately so: a
// client that did not ask for a stream is not owed one, and the alternative —
// buffering for a consumer that may never arrive — is an unbounded queue on
// a connection that lives as long as the credential does.
func (c *Conn) fanout(n *mcp.Notification) {
	c.mu.Lock()
	subs := make([]*Subscription, 0, len(c.subs))
	for s := range c.subs {
		subs = append(subs, s)
	}
	c.mu.Unlock()

	if len(subs) == 0 {
		c.g.log.Debug("dropping gateway notification (nobody subscribed)", "method", n.Method)
		return
	}
	for _, s := range subs {
		s.offer(n)
	}
}

// dropSubscription forgets one subscription. Called from Subscription.Close,
// which is why it must not be called while holding c.mu.
func (c *Conn) dropSubscription(s *Subscription) {
	c.mu.Lock()
	delete(c.subs, s)
	c.mu.Unlock()
}

// closeSubscriptions ends every live subscription. Collected under the lock
// and closed outside it: Subscription.Close reaches back into dropSubscription
// for the same mutex.
func (c *Conn) closeSubscriptions() {
	c.mu.Lock()
	subs := make([]*Subscription, 0, len(c.subs))
	for s := range c.subs {
		subs = append(subs, s)
	}
	clear(c.subs)
	c.mu.Unlock()
	for _, s := range subs {
		s.Close()
	}
}
