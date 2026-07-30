// Package event is the in-process event bus of the daemon
// (docs/modules/config.md, internal/event) plus the event mergers: a 50ms window
// coalescer for change storms and a 750ms "settled" debouncer for scan-style
// event streams. Standard library only — this package sits below every
// business package and must stay dependency-free.
//
// Delivery contract (the whole point, stated once): Publish NEVER blocks.
// A slow consumer loses events (counted per subscription) instead of
// stalling the daemon. Consumers therefore must treat the bus as a change
// NOTIFICATION channel, not a change LOG: on drop (or reconnect) they
// re-read authoritative state. Failure direction: losing a notification is
// recoverable by re-reading; blocking the publisher is not.
package event

import (
	"sync"
	"sync/atomic"
)

// Topic names one event stream. Topics are defined by producer packages
// (e.g. internal/session); the bus itself imposes no registry.
type Topic string

// Event is one published notification. Payload is producer-defined and must
// be treated as immutable by all subscribers (the same value is fanned out
// to every subscription).
type Event struct {
	Topic   Topic
	Key     string // producer-defined identity, e.g. a session ID or server ID
	Payload any
}

// DefaultBuffer is the per-subscription channel capacity used when
// Subscribe is called with buffer <= 0.
const DefaultBuffer = 16

// Bus is a topic-filtered fan-out bus. The zero value is not usable; use
// NewBus. All methods are safe for concurrent use.
type Bus struct {
	mu   sync.RWMutex
	subs map[*Subscription]struct{}
}

// NewBus returns an empty bus.
func NewBus() *Bus {
	return &Bus{subs: make(map[*Subscription]struct{})}
}

// Subscription is one subscriber's bounded delivery queue.
type Subscription struct {
	bus     *Bus
	topics  map[Topic]struct{} // nil = all topics
	ch      chan Event
	dropped atomic.Uint64
	once    sync.Once
}

// Subscribe registers a subscription for the given topics (none = all
// topics) with a bounded buffer of the given capacity (<= 0 uses
// DefaultBuffer). The caller must eventually call Close.
func (b *Bus) Subscribe(buffer int, topics ...Topic) *Subscription {
	if buffer <= 0 {
		buffer = DefaultBuffer
	}
	s := &Subscription{bus: b, ch: make(chan Event, buffer)}
	if len(topics) > 0 {
		s.topics = make(map[Topic]struct{}, len(topics))
		for _, t := range topics {
			s.topics[t] = struct{}{}
		}
	}
	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	return s
}

// Publish fans ev out to every matching subscription. It never blocks: a
// full subscription buffer drops the event and increments that
// subscription's drop counter (see package doc for the recovery contract).
func (b *Bus) Publish(ev Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for s := range b.subs {
		if s.topics != nil {
			if _, ok := s.topics[ev.Topic]; !ok {
				continue
			}
		}
		select {
		case s.ch <- ev:
		default:
			// Slow consumer: drop + count, never block the publisher.
			s.dropped.Add(1)
		}
	}
}

// Events returns the delivery channel. It is closed by Close.
func (s *Subscription) Events() <-chan Event { return s.ch }

// Dropped reports how many events were discarded because the buffer was
// full. A non-zero value means the subscriber has missed state changes and
// must re-read authoritative state.
func (s *Subscription) Dropped() uint64 { return s.dropped.Load() }

// Close unsubscribes and closes the delivery channel. Idempotent.
// Invariant: the subscription is removed from the bus (under the write
// lock) BEFORE the channel is closed, and Publish sends only under the read
// lock — so no send can race the close.
func (s *Subscription) Close() {
	s.once.Do(func() {
		s.bus.mu.Lock()
		delete(s.bus.subs, s)
		s.bus.mu.Unlock()
		close(s.ch)
	})
}
