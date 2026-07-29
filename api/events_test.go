package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

// sseTestHandler serves one SSE event per connection and records the
// Last-Event-ID header of every connection.
type sseTestHandler struct {
	mu      sync.Mutex
	conns   int
	lastIDs []string
	topics  []string
}

func (h *sseTestHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.conns++
	n := h.conns
	h.lastIDs = append(h.lastIDs, r.Header.Get("Last-Event-ID"))
	h.topics = append(h.topics, r.URL.Query().Get("topics"))
	h.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fl := w.(http.Flusher)
	// One event per connection, id = connection ordinal; then close the
	// stream to force the client to reconnect.
	_, _ = fmt.Fprintf(w, "event: servers\nid: %d\ndata: {\"topic\":\"servers\",\"kind\":\"changed\",\"rev\":%d,\"payload\":null}\n\n", n, n)
	fl.Flush()
}

func TestSubscribeReconnectsWithLastEventID(t *testing.T) {
	h := &sseTestHandler{}
	mux := http.NewServeMux()
	mux.Handle("GET /v1/events", h)
	c := New(newTestDaemon(t, mux))
	defer c.Close()
	c.Events.retryMin = 10 * time.Millisecond
	c.Events.retryMax = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ch, err := c.Events.Subscribe(ctx, "servers", "sessions")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	var got []Event
	for ev := range ch {
		got = append(got, ev)
		if len(got) == 3 {
			cancel() // stop the subscription; channel must close
		}
	}
	if len(got) < 3 {
		t.Fatalf("received %d events, want 3 (across reconnects)", len(got))
	}
	for i, ev := range got[:3] {
		if ev.Topic != "servers" || ev.Kind != "changed" || ev.Rev != uint64(i+1) {
			t.Errorf("event %d = %+v, want servers/changed rev %d", i, ev, i+1)
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lastIDs[0] != "" {
		t.Errorf("first connection sent Last-Event-ID %q, want none", h.lastIDs[0])
	}
	// Reconnects must resume from the id of the last received event.
	for i := 1; i < 3 && i < len(h.lastIDs); i++ {
		if want := fmt.Sprint(i); h.lastIDs[i] != want {
			t.Errorf("reconnect %d sent Last-Event-ID %q, want %q", i, h.lastIDs[i], want)
		}
	}
	for _, topics := range h.topics[:3] {
		if topics != "servers,sessions" {
			t.Errorf("topics query = %q, want servers,sessions", topics)
		}
	}
}

func TestSubscribeInitialConnectFailure(t *testing.T) {
	// Daemon rejects the subscription with a structured error: it must
	// surface synchronously from Subscribe.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"error":{"code":"E_BAD_TOPIC","message":"unknown topic 'x'"}}`))
	})
	c := New(newTestDaemon(t, mux))
	defer c.Close()

	_, err := c.Events.Subscribe(context.Background(), "x")
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code != "E_BAD_TOPIC" {
		t.Fatalf("want *api.Error E_BAD_TOPIC, got %v", err)
	}
}

func TestSubscribeChannelClosesOnCancel(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select { // hold the stream open, no events
		case <-block:
		case <-r.Context().Done():
		}
	})
	c := New(newTestDaemon(t, mux))
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := c.Events.Subscribe(ctx, "servers")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	cancel()
	select {
	case _, open := <-ch:
		if open {
			t.Fatal("received an event after cancel, want channel close")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("channel did not close after context cancellation")
	}
}
