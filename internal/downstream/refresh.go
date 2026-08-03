package downstream

import (
	"context"
	"errors"
	"sync"

	"github.com/dinstein/agent-hub/internal/calllog"
	"github.com/dinstein/agent-hub/internal/mcp"
)

// listMerge is the leader/waiter merger for tools/list ("concurrent tools/list
// calls are merged leader/waiter"). On a slow stdio server several
// discovery paths — the gateway's own refresh, a list_changed reaction, a
// CLI inspect — routinely ask for the tool list at the same moment; without
// merging they queue behind each other on the single owner goroutine and
// every one of them pays the full latency.
//
// Semantics:
//
//   - the FIRST caller becomes the leader and performs the round trip;
//   - callers arriving while a round trip is IN FLIGHT become waiters and
//     receive the leader's outcome — they never issue their own call;
//   - a waiter whose own context ends stops waiting immediately and returns
//     its context error. The leader is unaffected: its round trip belongs to
//     the server, not to whoever happened to start it.
//
// Failure direction: sharing an outcome means a waiter can inherit the
// leader's error. That is correct for a refresh (both would have hit the
// same connection); it is why this merger is used for tools/list ONLY and
// never for tools/call, which is not idempotent and must never be shared.
type listMerge struct {
	mu     sync.Mutex
	inFlt  bool
	done   chan struct{} // closed by the leader when the round trip ends
	result error         // the leader's outcome; read only after done closes
}

// join returns leader=true for the caller that must perform the round trip.
// Waiters get the channel to wait on and read result after it closes.
func (m *listMerge) join() (leader bool, done chan struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.inFlt {
		return false, m.done
	}
	m.inFlt = true
	m.done = make(chan struct{})
	return true, m.done
}

// finish publishes the leader's outcome and releases every waiter. The
// leader must always call it, including on panic paths (callers use defer).
func (m *listMerge) finish(done chan struct{}, err error) {
	m.mu.Lock()
	m.inFlt = false
	m.result = err
	m.done = nil
	m.mu.Unlock()
	close(done)
}

// outcome reads the last published leader outcome. Only meaningful after
// the corresponding done channel has closed.
func (m *listMerge) outcome() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.result
}

// refreshMerged runs one tools/list refresh under the leader/waiter merger.
//
// A waiter that inherits the LEADER's cancellation (the leader's caller went
// away mid-flight) promotes itself and retries once as a leader — otherwise
// a caller with a perfectly live context would be failed by someone else's
// Ctrl-C. The retry is bounded at one: two cancellations in a row are
// reported, never looped on.
func (s *Server) refreshMerged(ctx context.Context) error {
	var last error
	for attempt := 0; attempt < 2; attempt++ {
		leader, done := s.listMerge.join()
		if leader {
			return s.refreshAsLeader(ctx, done)
		}
		select {
		case <-done:
			last = s.listMerge.outcome()
			if last != nil && ctx.Err() == nil && isContextError(last) {
				continue // the leader was cancelled, this caller was not
			}
			return last
		case <-ctx.Done():
			// This waiter gives up; the leader keeps going for everyone else.
			return context.Cause(ctx)
		case <-s.lifeCtx.Done():
			return ErrServerClosed
		}
	}
	return last
}

// refreshAsLeader performs the round trip and publishes its outcome.
func (s *Server) refreshAsLeader(ctx context.Context, done chan struct{}) (err error) {
	defer func() { s.listMerge.finish(done, err) }()
	_, err = s.enqueue(ctx, kindRefresh, Origin{Cause: calllog.CauseList}, mcp.MethodToolsList, nil)
	return err
}

// isContextError reports whether err was produced by a cancelled or expired
// context rather than by the connection.
func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
