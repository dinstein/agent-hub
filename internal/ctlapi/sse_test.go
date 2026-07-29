package ctlapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/api"
	"github.com/dinstein/agent-hub/internal/event"
	"github.com/dinstein/agent-hub/internal/session"
)

func recvEvent(t *testing.T, ch <-chan api.Event, timeout time.Duration) (api.Event, bool) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		return ev, ok
	case <-time.After(timeout):
		return api.Event{}, false
	}
}

func TestEventsSessionLifecycle(t *testing.T) {
	client, env := startServer(t, nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ch, err := client.Events.Subscribe(ctx, TopicSessions)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	s := openSession(t, env.mgr, "cursor")
	ev, ok := recvEvent(t, ch, 2*time.Second)
	if !ok {
		t.Fatal("no opened event")
	}
	if ev.Topic != TopicSessions || ev.Kind != "opened" {
		t.Fatalf("event = %+v", ev)
	}
	var info api.SessionInfo
	if err := json.Unmarshal(ev.Payload, &info); err != nil {
		t.Fatalf("payload: %v (%s)", err, ev.Payload)
	}
	if info.ID != string(s.ID) || info.ClientID != "cursor" {
		t.Errorf("payload = %+v", info)
	}

	env.mgr.Close(s.ID)
	ev, ok = recvEvent(t, ch, 2*time.Second)
	if !ok {
		t.Fatal("no closed event")
	}
	if ev.Kind != "closed" {
		t.Errorf("event = %+v", ev)
	}
	if !strings.Contains(string(ev.Payload), `"reason":"closed"`) {
		t.Errorf("closed payload = %s", ev.Payload)
	}
}

// TestEventsTopicFilter: a subscription to sessions only must not receive
// servers events.
func TestEventsTopicFilter(t *testing.T) {
	client, env := startServer(t, func(o *Options) { o.CoalesceWindow = 20 * time.Millisecond })

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ch, err := client.Events.Subscribe(ctx, TopicSessions)
	if err != nil {
		t.Fatal(err)
	}

	env.bus.Publish(event.Event{Topic: "server.changed", Key: "github"})
	s := openSession(t, env.mgr, "cursor")

	ev, ok := recvEvent(t, ch, 2*time.Second)
	if !ok {
		t.Fatal("no event")
	}
	if ev.Topic != TopicSessions {
		t.Fatalf("leaked topic %q through the filter", ev.Topic)
	}
	_ = s
}

// TestEventsServerStormCoalesced: K rapid server-change notifications
// collapse into ONE SSE frame whose payload is the full server list
// (built lazily, embedded Health, byte-identical to GET /v1/servers).
func TestEventsServerStormCoalesced(t *testing.T) {
	states := fakeStates{"github": {Conn: ConnConnected, Tools: 3}}
	client, env := startServer(t, func(o *Options) {
		o.States = states
		o.CoalesceWindow = 100 * time.Millisecond
	})
	seedServer(t, env.reg, "github", true)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ch, err := client.Events.Subscribe(ctx, TopicServers)
	if err != nil {
		t.Fatal(err)
	}
	// Give the server-side subscription a moment to attach.
	time.Sleep(50 * time.Millisecond)

	for range 5 {
		env.bus.Publish(event.Event{Topic: "server.changed", Key: "github"})
	}

	ev, ok := recvEvent(t, ch, 2*time.Second)
	if !ok {
		t.Fatal("no coalesced event")
	}
	if ev.Topic != TopicServers || ev.Kind != "changed" {
		t.Fatalf("event = %+v", ev)
	}
	if ev.Rev != env.reg.Snapshot().Generation {
		t.Errorf("rev = %d, want %d", ev.Rev, env.reg.Snapshot().Generation)
	}
	var servers []api.Server
	if err := json.Unmarshal(ev.Payload, &servers); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if len(servers) != 1 || servers[0].ID != "github" || servers[0].Tools != 3 {
		t.Errorf("payload = %+v", servers)
	}
	if servers[0].Health.Level != api.HealthLevelHealthy {
		t.Errorf("health not embedded: %+v", servers[0].Health)
	}

	// The storm produced exactly one frame: nothing else arrives.
	if extra, ok := recvEvent(t, ch, 300*time.Millisecond); ok {
		t.Errorf("storm was not coalesced, extra event %+v", extra)
	}
}

// TestEventsLastEventIDSync: resuming with a stale (or unparseable) id
// triggers best-effort sync events for the state-backed subscribed topics.
func TestEventsLastEventIDSync(t *testing.T) {
	client, env := startServer(t, nil)
	seedServer(t, env.reg, "github", true)
	openSession(t, env.mgr, "cursor")

	// Generate at least one delivered frame so the global sequence is > 0.
	ctx1, cancel1 := context.WithCancel(t.Context())
	ch, err := client.Events.Subscribe(ctx1, TopicSessions)
	if err != nil {
		t.Fatal(err)
	}
	openSession(t, env.mgr, "zed")
	if _, ok := recvEvent(t, ch, 2*time.Second); !ok {
		t.Fatal("no priming event")
	}
	cancel1()

	// Reconnect claiming an old id: expect sync frames.
	hc := rawClient(env.sock)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		"http://agenthub/v1/events?topics=servers,sessions", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Last-Event-ID", "0")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type = %q", ct)
	}

	got := map[string]api.Event{}
	sc := bufio.NewScanner(resp.Body)
	deadline := time.AfterFunc(3*time.Second, func() { _ = resp.Body.Close() })
	defer deadline.Stop()
	for len(got) < 2 && sc.Scan() {
		line := sc.Text()
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			var ev api.Event
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				t.Fatalf("bad frame %q: %v", data, err)
			}
			got[ev.Topic] = ev
		}
	}
	srvEv, ok := got[TopicServers]
	if !ok || srvEv.Kind != "sync" {
		t.Fatalf("no servers sync event: %+v", got)
	}
	var servers []api.Server
	if err := json.Unmarshal(srvEv.Payload, &servers); err != nil || len(servers) != 1 {
		t.Errorf("servers sync payload = %s (%v)", srvEv.Payload, err)
	}
	sesEv, ok := got[TopicSessions]
	if !ok || sesEv.Kind != "sync" {
		t.Fatalf("no sessions sync event: %+v", got)
	}
	var sessions []api.SessionInfo
	if err := json.Unmarshal(sesEv.Payload, &sessions); err != nil || len(sessions) != 2 {
		t.Errorf("sessions sync payload = %s (%v)", sesEv.Payload, err)
	}
}

func TestEventsUnknownTopicRejected(t *testing.T) {
	client, _ := startServer(t, nil)
	_, err := client.Events.Subscribe(t.Context(), "gossip")
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v", err)
	}
	if apiErr.Status != http.StatusBadRequest || apiErr.Code != CodeBadRequest {
		t.Errorf("status=%d code=%s", apiErr.Status, apiErr.Code)
	}
	if apiErr.Hint == "" {
		t.Error("no hint listing valid topics")
	}
}

// TestEventsOverlayChange: a scope mutation through the API produces an
// overlay event on the stream (wire shape pinned).
func TestEventsOverlayChange(t *testing.T) {
	client, env := startServer(t, nil)
	s := openSession(t, env.mgr, "cursor")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ch, err := client.Events.Subscribe(ctx, TopicSessions)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // let the server-side subscription attach

	if err := client.Sessions.SetScope(t.Context(), string(s.ID), api.ScopeNarrow{
		Tools: map[string][]string{"gh": {"a"}},
	}); err != nil {
		t.Fatal(err)
	}
	ev, ok := recvEvent(t, ch, 2*time.Second)
	if !ok {
		t.Fatal("no overlay event")
	}
	if ev.Kind != "overlay" {
		t.Fatalf("event = %+v", ev)
	}
	var payload struct {
		ID      string `json:"id"`
		Version uint64 `json:"version"`
	}
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ID != string(s.ID) || payload.Version != 1 {
		t.Errorf("payload = %+v", payload)
	}
}

// TestSSETopicMapping pins the bus-topic -> SSE-topic classification,
// including that daemon-internal topics never leak.
func TestSSETopicMapping(t *testing.T) {
	cases := []struct {
		bus   event.Topic
		topic string
		kind  string
		ok    bool
	}{
		{session.TopicOpened, TopicSessions, "opened", true},
		{session.TopicClosed, TopicSessions, "closed", true},
		{session.TopicOverlay, TopicSessions, "overlay", true},
		{"server.changed", TopicServers, "changed", true},
		{"approval.pending", TopicApprovals, "pending", true},
		{"skill.updated", TopicSkills, "updated", true},
		{"activity.call", TopicActivity, "call", true},
		{"internal.debug", "", "", false},
		{"registry.write", "", "", false},
	}
	for _, tc := range cases {
		topic, kind, ok := sseTopicOf(tc.bus)
		if ok != tc.ok {
			t.Errorf("sseTopicOf(%q) ok = %v, want %v", tc.bus, ok, tc.ok)
			continue
		}
		if ok && (topic != tc.topic || kind != tc.kind) {
			t.Errorf("sseTopicOf(%q) = (%q, %q), want (%q, %q)", tc.bus, topic, kind, tc.topic, tc.kind)
		}
	}
}
