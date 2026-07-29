package event

import (
	"sync"
	"testing"
)

func TestBusTopicFilter(t *testing.T) {
	b := NewBus()
	only := b.Subscribe(8, "a")
	all := b.Subscribe(8)
	defer only.Close()
	defer all.Close()

	b.Publish(Event{Topic: "a", Key: "1"})
	b.Publish(Event{Topic: "b", Key: "2"})

	got := <-only.Events()
	if got.Topic != "a" || got.Key != "1" {
		t.Fatalf("filtered sub got %+v", got)
	}
	select {
	case ev := <-only.Events():
		t.Fatalf("filtered sub leaked %+v", ev)
	default:
	}

	if ev := <-all.Events(); ev.Topic != "a" {
		t.Fatalf("all sub first = %+v", ev)
	}
	if ev := <-all.Events(); ev.Topic != "b" {
		t.Fatalf("all sub second = %+v", ev)
	}
}

func TestBusSlowConsumerDropsAndCounts(t *testing.T) {
	b := NewBus()
	s := b.Subscribe(1)
	defer s.Close()

	for range 5 {
		b.Publish(Event{Topic: "t", Key: "k"}) // must never block
	}
	if got := s.Dropped(); got != 4 {
		t.Fatalf("Dropped = %d, want 4", got)
	}
	<-s.Events()
	select {
	case ev := <-s.Events():
		t.Fatalf("unexpected buffered event %+v", ev)
	default:
	}
}

func TestBusSubscribeDefaultBuffer(t *testing.T) {
	b := NewBus()
	s := b.Subscribe(0)
	defer s.Close()
	for range DefaultBuffer {
		b.Publish(Event{Topic: "t"})
	}
	if got := s.Dropped(); got != 0 {
		t.Fatalf("Dropped = %d within default buffer", got)
	}
}

func TestSubscriptionCloseIsIdempotentAndStopsDelivery(t *testing.T) {
	b := NewBus()
	s := b.Subscribe(4)
	s.Close()
	s.Close() // must not panic
	b.Publish(Event{Topic: "t"})
	if _, open := <-s.Events(); open {
		t.Fatal("channel still open after Close")
	}
}

// TestBusConcurrentPublishClose exercises the remove-before-close invariant
// under the race detector.
func TestBusConcurrentPublishClose(t *testing.T) {
	b := NewBus()
	var wg sync.WaitGroup
	for range 8 {
		s := b.Subscribe(1, "t")
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				b.Publish(Event{Topic: "t"})
			}
		}()
		go func() {
			defer wg.Done()
			s.Close()
		}()
	}
	wg.Wait()
}
