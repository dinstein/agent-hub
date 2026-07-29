package oauthflow

import (
	"context"
	"sync"
)

// Group collapses concurrent calls sharing a key into one execution. It is
// provided here (rather than pulled in as a dependency) because ruling A.2
// #10 makes it load-bearing: when the daemon is up it is the ONLY writer of
// OAuth credentials, so in-process deduplication is the entire concurrency
// story for refreshes. A refresh token spent twice concurrently is spent —
// the second exchange fails and, on rotating providers, the first one's
// result has already been invalidated.
//
// The zero Group is ready to use. Group is safe for concurrent use.
//
// Differences from the classic singleflight worth knowing:
//
//   - Waiters honour their own context. A caller whose context is cancelled
//     returns immediately with ctx.Err() instead of being pinned to the
//     leader's lifetime. The leader keeps running so the other waiters
//     still get an answer.
//   - The leader runs under the context of whoever started it. A refresh
//     must not be cancelled by a caller wandering off, so the daemon passes
//     a background-derived context.
type Group[T any] struct {
	mu    sync.Mutex
	calls map[string]*groupCall[T]
}

type groupCall[T any] struct {
	done chan struct{}
	val  T
	err  error
}

// Do runs fn for key, sharing the result with every call that arrives while
// it is in flight. shared reports whether this caller joined an existing
// call rather than starting one — the daemon uses it to log "N callers, 1
// refresh".
func (g *Group[T]) Do(ctx context.Context, key string, fn func(context.Context) (T, error)) (val T, shared bool, err error) {
	g.mu.Lock()
	if g.calls == nil {
		g.calls = make(map[string]*groupCall[T])
	}
	if c, ok := g.calls[key]; ok {
		g.mu.Unlock()
		select {
		case <-c.done:
			return c.val, true, c.err
		case <-ctx.Done():
			var zero T
			return zero, true, ctx.Err()
		}
	}
	c := &groupCall[T]{done: make(chan struct{})}
	g.calls[key] = c
	g.mu.Unlock()

	// The key is removed BEFORE the result is published, so a caller that
	// arrives immediately after close(done) starts a fresh call instead of
	// joining a finished one.
	func() {
		defer func() {
			g.mu.Lock()
			delete(g.calls, key)
			g.mu.Unlock()
			close(c.done)
		}()
		c.val, c.err = fn(ctx)
	}()
	return c.val, false, c.err
}
