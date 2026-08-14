package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// headerLastEventID resumes an SSE stream after reconnect.
const headerLastEventID = "Last-Event-ID"

// Subscribable SSE topics.
//
// CONTRACT: mirrors the closed set in internal/ctlapi (api must not import
// internal/*, docs/conventions.md#dependency-directions rule 1). An unknown topic is rejected by the
// daemon rather than silently ignored, so these names are matched, not free
// text.
const (
	// TopicServers carries server list/health changes; its payload is the
	// full []Server with embedded Health, byte-identical to Servers.List.
	TopicServers = "servers"
	// TopicSessions carries session lifecycle changes.
	TopicSessions = "sessions"
	// TopicSkills carries skills library/install changes.
	TopicSkills = "skills"
)

// EventsService subscribes to the daemon event stream
// (GET /v1/events?topics=...).
type EventsService struct {
	c *Client
	// retryMin/retryMax bound the reconnect backoff (exponential,
	// reset on a successful reconnect). Overridable in tests.
	retryMin time.Duration
	retryMax time.Duration
}

// Subscribe opens the SSE stream for the given topics ("servers",
// "sessions", "skills"; empty = all) and returns a channel of
// decoded events.
//
// The set is CLOSED at the daemon: an unlisted name is a 400, not a
// subscription that quietly delivers nothing. So a topic retired on the
// daemon side must be retired here in the same change — leaving the
// constant behind does not degrade to "that topic is empty", it takes the
// whole subscription down with it, including the unrelated topics named in
// the same call. TestAPITopicsMatchTheServedSet (internal/ctlapi) enforces
// the agreement in both directions; it lives there because this package may
// not import internal/*.
//
// The initial connection is made synchronously so callers learn
// immediately when the daemon is down. Afterwards a goroutine keeps the
// stream alive: on any stream error it reconnects with exponential
// backoff, resuming via Last-Event-ID so the daemon can replay missed
// events. The channel is closed only when ctx is done — consumers can
// range over it and treat channel close as "subscription ended".
//
// Events are notifications, not snapshots: for registry-backed topics the
// consumer re-reads state and applies it when the read generation >= the
// applied one (docs/conventions.md#the-hot-reload-path) — never "equal to the event Rev".
func (s *EventsService) Subscribe(ctx context.Context, topics ...string) (<-chan Event, error) {
	body, err := s.connect(ctx, topics, "")
	if err != nil {
		return nil, err
	}
	ch := make(chan Event, 16)
	go s.pump(ctx, body, topics, ch)
	return ch, nil
}

// connect performs one SSE connection attempt.
func (s *EventsService) connect(ctx context.Context, topics []string, lastID string) (io.ReadCloser, error) {
	u := baseURL + apiPrefix + "/events"
	if len(topics) > 0 {
		q := url.Values{"topics": {strings.Join(topics, ",")}}
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	s.c.setCommonHeaders(ctx, req)
	req.Header.Set("Accept", "text/event-stream")
	if lastID != "" {
		req.Header.Set(headerLastEventID, lastID)
	}
	resp, err := s.c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	ct := resp.Header.Get("Content-Type")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(ct, "text/event-stream") {
		// Not a stream: decode as an envelope so server errors pass
		// through; decodeEnvelope fails closed on anything else.
		defer func() { _ = resp.Body.Close() }()
		if err := decodeEnvelope(resp, nil); err != nil {
			return nil, err
		}
		return nil, &Error{
			ErrorBody: ErrorBody{Code: ErrCodeBadResponse,
				Message: "expected text/event-stream, got " + ct},
			Status:    resp.StatusCode,
			RequestID: resp.Header.Get(HeaderRequestID),
		}
	}
	return resp.Body, nil
}

// pump reads the stream, delivering events and reconnecting on failure,
// until ctx is done. It owns closing ch.
func (s *EventsService) pump(ctx context.Context, body io.ReadCloser, topics []string, ch chan<- Event) {
	defer close(ch)
	lastID := ""
	backoff := s.retryMin
	for {
		lastID = s.readStream(ctx, body, lastID, ch)
		_ = body.Close()
		if ctx.Err() != nil {
			return
		}
		// Reconnect loop: exponential backoff, reset on success.
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			nb, err := s.connect(ctx, topics, lastID)
			if err == nil {
				body = nb
				backoff = s.retryMin
				break
			}
			if ctx.Err() != nil {
				return
			}
			backoff = min(backoff*2, s.retryMax)
		}
	}
}

// readStream parses one connection until it fails or ctx is done,
// returning the last event id seen (for resumption). Cancelling ctx
// closes the response body via the request context, which unblocks the
// read.
func (s *EventsService) readStream(ctx context.Context, body io.Reader, lastID string, ch chan<- Event) string {
	p := newSSEParser(body)
	p.lastID = lastID
	for {
		name, data, err := p.next()
		if err != nil {
			return p.lastID
		}
		var ev Event
		if err := json.Unmarshal(data, &ev); err != nil {
			// A malformed frame is skipped, not fatal: the stream stays
			// usable and consumers re-sync from state reads anyway.
			continue
		}
		if ev.Topic == "" {
			ev.Topic = name
		}
		select {
		case ch <- ev:
		case <-ctx.Done():
			return p.lastID
		}
	}
}
