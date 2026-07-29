package ctlapi

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/api"
	"github.com/dinstein/agent-hub/internal/event"
)

// event merger, second half: scan-style topics are DEBOUNCED
// (every event resets the window) so a whole lifecycle collapses into one
// terminal frame. The first half — the 50ms servers-topic coalescer — is
// pinned in sse_test.go.

// A scan lifecycle of K events must produce exactly ONE settled frame, and
// that frame must reflect the LAST event of the burst.
func TestSkillsScanSettlesIntoOneTerminalEvent(t *testing.T) {
	client, env := startServer(t, func(o *Options) { o.SettleWindow = 60 * time.Millisecond })

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ch, err := client.Events.Subscribe(ctx, TopicSkills)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// A lifecycle stream: started, four progress ticks, finished. Each event
	// arrives well inside the window, so each resets it.
	kinds := []string{"scan.started", "scan.progress", "scan.progress", "scan.progress", "scan.finished"}
	for _, kind := range kinds {
		env.bus.Publish(event.Event{
			Topic:   event.Topic("skill." + kind),
			Key:     "library",
			Payload: json.RawMessage(`{"kind":"` + kind + `"}`),
		})
		time.Sleep(10 * time.Millisecond)
	}

	ev, ok := recvEvent(t, ch, 3*time.Second)
	if !ok {
		t.Fatal("no settled event arrived")
	}
	if ev.Topic != TopicSkills {
		t.Fatalf("topic = %q, want %q", ev.Topic, TopicSkills)
	}
	if ev.Kind != settledKind {
		t.Fatalf("kind = %q, want %q (the whole lifecycle must collapse)", ev.Kind, settledKind)
	}
	var body struct {
		Key     string          `json:"key"`
		Kind    string          `json:"kind"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(ev.Payload, &body); err != nil {
		t.Fatalf("settled payload %s: %v", ev.Payload, err)
	}
	if body.Key != "library" {
		t.Errorf("settled key = %q, want %q", body.Key, "library")
	}
	if body.Kind != "scan.finished" {
		t.Errorf("settled kind = %q, want %q (the LAST event of the burst)", body.Kind, "scan.finished")
	}

	// Nothing else may follow: the intermediate frames were replaced, not
	// merely delayed.
	if extra, more := recvEvent(t, ch, 300*time.Millisecond); more {
		t.Fatalf("a second frame arrived after the settled one: %+v", extra)
	}
}

// Two different scan keys settle independently — debouncing must not merge
// unrelated subjects into one another's terminal state.
func TestSettlerKeepsScanKeysSeparate(t *testing.T) {
	client, env := startServer(t, func(o *Options) { o.SettleWindow = 40 * time.Millisecond })

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ch, err := client.Events.Subscribe(ctx, TopicSkills)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	for _, key := range []string{"alpha", "beta"} {
		env.bus.Publish(event.Event{
			Topic:   event.Topic("skill.scan.finished"),
			Key:     key,
			Payload: json.RawMessage(`{}`),
		})
	}

	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		ev, ok := recvEvent(t, ch, 3*time.Second)
		if !ok {
			t.Fatalf("only %d settled events arrived, want 2", i)
		}
		var body struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal(ev.Payload, &body); err != nil {
			t.Fatalf("payload %s: %v", ev.Payload, err)
		}
		seen[body.Key] = true
	}
	if !seen["alpha"] || !seen["beta"] {
		t.Fatalf("settled keys = %v, want both alpha and beta", seen)
	}
}

// A non-scan topic must NOT be debounced: "session opened" and "session
// closed" are two different facts, and collapsing them would lose one.
func TestSettlerDoesNotSwallowPerEventTopics(t *testing.T) {
	client, env := startServer(t, func(o *Options) { o.SettleWindow = time.Hour })

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ch, err := client.Events.Subscribe(ctx, TopicSessions)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	s := openSession(t, env.mgr, "cursor")
	env.mgr.Close(s.ID)

	var kinds []string
	for i := 0; i < 2; i++ {
		ev, ok := recvEvent(t, ch, 3*time.Second)
		if !ok {
			t.Fatalf("only %d session events arrived (settler swallowed them): %v", i, kinds)
		}
		kinds = append(kinds, ev.Kind)
	}
	if kinds[0] != "opened" || kinds[1] != "closed" {
		t.Fatalf("kinds = %v, want [opened closed] delivered per event", kinds)
	}
}

// The settled topic set is a CLOSED list: adding a topic to it changes the
// wire contract for that topic's consumers, so the list is pinned by test.
func TestSettledTopicsAreExactly(t *testing.T) {
	want := map[string]bool{TopicSkills: true}
	if len(settledTopics) != len(want) {
		t.Fatalf("settledTopics = %v, want %v", settledTopics, want)
	}
	for topic := range want {
		if !settledTopics[topic] {
			t.Errorf("topic %q missing from settledTopics", topic)
		}
	}
	// Every settled topic must also be a real SSE topic.
	for topic := range settledTopics {
		if !sseTopics[topic] {
			t.Errorf("settled topic %q is not a subscribable SSE topic", topic)
		}
	}
	_ = api.Event{} // the settled frame travels as an ordinary api.Event
}
