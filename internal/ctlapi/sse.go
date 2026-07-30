package ctlapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dinstein/agent-hub/api"
	"github.com/dinstein/agent-hub/internal/event"
	"github.com/dinstein/agent-hub/internal/session"
)

// SSE topics exposed on /v1/events (api.EventsService mirrors this list).
const (
	TopicServers  = "servers"
	TopicSessions = "sessions"
	TopicActivity = "activity"
	TopicSkills   = "skills"
)

// sseTopics is the closed set of subscribable topics.
var sseTopics = map[string]bool{
	TopicServers:  true,
	TopicSessions: true,
	TopicActivity: true,
	TopicSkills:   true,
}

// busTopicPrefixMap maps internal bus topic name prefixes (the segment
// before the first '.') to SSE topics. Bus topics outside this table are
// daemon-internal and never leave the process.
var busTopicPrefixMap = map[string]string{
	"session":  TopicSessions,
	"server":   TopicServers,
	"activity": TopicActivity,
	"skill":    TopicSkills,
}

// sseTopicOf classifies one bus topic: SSE topic + event kind (the bus
// topic's suffix, e.g. "session.opened" -> ("sessions", "opened")).
func sseTopicOf(t event.Topic) (topic, kind string, ok bool) {
	name := string(t)
	prefix, kind, found := strings.Cut(name, ".")
	if !found {
		kind = "changed"
	}
	topic, ok = busTopicPrefixMap[prefix]
	return topic, kind, ok
}

// sseBufferedFrames bounds the per-connection queue between the coalescer
// timer goroutine and the writing goroutine. Overflow drops the frame —
// the bus contract already requires consumers to re-read state on loss.
const sseBufferedFrames = 32

// settledKind is the event kind of a terminal, debounced scan event. It
// replaces the whole "started / progress / … / finished" stream of a scan
// with one frame once the stream has been quiet for the settle window
// ("debounce scan-style events with settled: one terminal event after 750ms
// replaces the entire lifecycle event stream").
const settledKind = "settled"

// settledTopics is the closed set of SCAN-STYLE topics: streams whose
// intermediate frames carry no decision value for a frontend, only the
// terminal state does. Everything else passes through per event — a session
// open and a session close are two different facts and must not be merged
// into one.
var settledTopics = map[string]bool{
	TopicSkills: true,
}

// frame is one ready-to-write SSE event.
type frame struct {
	topic string
	ev    api.Event
}

// handleEvents implements GET /v1/events: an SSE stream of api.Event
// frames filtered by ?topics=a,b (empty = all).
//
// Wiring: the handler subscribes to the daemon bus
// and pushes each mapped event out. The `servers` topic goes through a
// per-connection 50ms coalescer with a LAZILY built payload: a change storm
// of K bus events becomes one SSE frame whose payload — the full server
// list with embedded Health — is built exactly once at fire time. Other
// topics pass through directly (their events are individually meaningful).
//
// Last-Event-ID is best-effort by design: events are notifications, not a
// replayable log. Frame ids are globally monotonic across connections; when
// a client resumes with an id older than the current sequence, the handler
// emits one fresh "sync" event per subscribed state-backed topic (servers,
// sessions) so the client re-reads state instead of pretending nothing was
// missed. Unparseable ids are treated as stale (sync), never trusted.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	reqID := requestIDFrom(r.Context())
	topics, err := parseTopics(r.URL.Query().Get("topics"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, err.Error(),
			"valid topics: servers, sessions, activity, skills", reqID)
		return
	}
	rc := http.NewResponseController(w)

	sub := s.opts.Bus.Subscribe(event.DefaultBuffer)
	defer sub.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_ = rc.Flush()

	out := make(chan frame, sseBufferedFrames)
	enqueue := func(f frame) {
		select {
		case out <- f:
		default:
			// Slow consumer: drop, never block the coalescer timer. The
			// client recovers by re-reading state (bus contract).
		}
	}
	coal := event.NewCoalescer(func(ev event.Event) {
		payload, ok := ev.Payload.([]api.Server)
		if !ok {
			return
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return
		}
		enqueue(frame{topic: TopicServers, ev: api.Event{
			Topic:   TopicServers,
			Kind:    "changed",
			Rev:     s.opts.Registry.Snapshot().Generation,
			Payload: raw,
		}})
	}, s.opts.CoalesceWindow)
	defer coal.Close()

	// Scan-style topics: every bus event RESETS a 750ms window, so a whole
	// scan lifecycle collapses into one terminal frame. The payload is built
	// lazily at fire time from the LAST event of the burst, so a K-event scan
	// costs one marshal, not K.
	settle := event.NewSettler(func(ev event.Event) {
		payload, ok := ev.Payload.(json.RawMessage)
		if !ok {
			return
		}
		enqueue(frame{topic: string(ev.Topic), ev: api.Event{
			Topic:   string(ev.Topic),
			Kind:    settledKind,
			Payload: payload,
		}})
	}, s.opts.SettleWindow)
	defer settle.Close()

	// Best-effort resume: anything but "I already have the latest id"
	// triggers sync events for the state-backed subscribed topics.
	if lastID := r.Header.Get("Last-Event-ID"); lastID != "" {
		cur := s.eventSeq.Load()
		if n, perr := strconv.ParseUint(lastID, 10, 64); perr != nil || n < cur {
			s.emitSyncFrames(topics, enqueue)
		}
	}

	var keepalive <-chan time.Time
	if s.opts.KeepAlive > 0 {
		t := time.NewTicker(s.opts.KeepAlive)
		defer t.Stop()
		keepalive = t.C
	}

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-keepalive:
			if _, err := fmt.Fprint(w, ":ka\n\n"); err != nil {
				return
			}
			_ = rc.Flush()
		case f := <-out:
			if !s.writeFrame(w, rc, topics, f) {
				return
			}
		case ev, ok := <-sub.Events():
			if !ok {
				return
			}
			topic, kind, mapped := sseTopicOf(ev.Topic)
			if !mapped || !subscribed(topics, topic) {
				continue
			}
			if topic == TopicServers {
				// Coalesce storms; payload built lazily, once per fire.
				coal.Add(TopicServers, "", func() any { return s.serverList() })
				continue
			}
			if settledTopics[topic] {
				// Debounce the whole lifecycle into one terminal frame. The
				// builder captures THIS event's payload; the merger keeps only
				// the latest builder, so the frame reflects the last state of
				// the burst.
				last := ev
				settle.Add(event.Topic(topic), ev.Key, func() any {
					raw, merr := json.Marshal(settledPayload(kind, last))
					if merr != nil {
						return json.RawMessage(nil)
					}
					return json.RawMessage(raw)
				})
				continue
			}
			f, ok := s.directFrame(topic, kind, ev)
			if !ok {
				continue
			}
			if !s.writeFrame(w, rc, topics, f) {
				return
			}
		}
	}
}

// emitSyncFrames queues one state snapshot event per subscribed
// state-backed topic (resume path).
func (s *Server) emitSyncFrames(topics map[string]bool, enqueue func(frame)) {
	if subscribed(topics, TopicServers) {
		if raw, err := json.Marshal(s.serverList()); err == nil {
			enqueue(frame{topic: TopicServers, ev: api.Event{
				Topic:   TopicServers,
				Kind:    "sync",
				Rev:     s.opts.Registry.Snapshot().Generation,
				Payload: raw,
			}})
		}
	}
	if subscribed(topics, TopicSessions) {
		if raw, err := json.Marshal(s.sessionList()); err == nil {
			enqueue(frame{topic: TopicSessions, ev: api.Event{
				Topic:   TopicSessions,
				Kind:    "sync",
				Payload: raw,
			}})
		}
	}
}

// settledPayload is the wire body of a terminal scan frame: the LAST bus
// event of the burst, tagged with the kind it settled on so a frontend can
// tell "scan finished ok" from "scan finished with an error" without having
// watched the intermediate frames it never received.
func settledPayload(lastKind string, ev event.Event) any {
	return struct {
		Key     string `json:"key,omitempty"`
		Kind    string `json:"kind"`
		Payload any    `json:"payload,omitempty"`
	}{Key: ev.Key, Kind: lastKind, Payload: ev.Payload}
}

// directFrame converts a pass-through bus event into a wire frame. Known
// session payload types get stable wire shapes; unknown payloads are
// marshaled as-is (producers own their wire compatibility).
func (s *Server) directFrame(topic, kind string, ev event.Event) (frame, bool) {
	var payload any
	switch p := ev.Payload.(type) {
	case session.Info:
		payload = apiSessionInfo(p)
	case session.Closed:
		payload = struct {
			Session api.SessionInfo `json:"session"`
			Reason  string          `json:"reason"`
		}{apiSessionInfo(p.Info), string(p.Reason)}
	case session.OverlayChanged:
		payload = struct {
			ID      string `json:"id"`
			Version uint64 `json:"version"`
		}{string(p.ID), p.Version}
	default:
		payload = ev.Payload
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		s.log.Warn("ctlapi: dropping unmarshalable event payload",
			"topic", string(ev.Topic), "err", err)
		return frame{}, false
	}
	return frame{topic: topic, ev: api.Event{Topic: topic, Kind: kind, Payload: raw}}, true
}

// writeFrame writes one SSE event block (id/event/data) and flushes.
// Returns false when the connection is gone.
func (s *Server) writeFrame(w http.ResponseWriter, rc *http.ResponseController, topics map[string]bool, f frame) bool {
	if !subscribed(topics, f.topic) {
		return true
	}
	data, err := json.Marshal(f.ev)
	if err != nil {
		return true
	}
	id := s.eventSeq.Add(1)
	if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", id, f.topic, data); err != nil {
		return false
	}
	return rc.Flush() == nil
}

// parseTopics validates ?topics=a,b. Empty means all topics; an unknown
// topic is a client error (fail loudly instead of silently streaming
// nothing).
func parseTopics(q string) (map[string]bool, error) {
	if q == "" {
		return nil, nil // nil = all topics
	}
	out := make(map[string]bool)
	for _, t := range strings.Split(q, ",") {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if !sseTopics[t] {
			return nil, fmt.Errorf("unknown topic %q", t)
		}
		out[t] = true
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// subscribed reports whether topic passes the filter (nil = all).
func subscribed(topics map[string]bool, topic string) bool {
	return topics == nil || topics[topic]
}
