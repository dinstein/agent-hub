package session

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/event"
)

// fakeClock is an injectable, advanceable clock.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// fakeLink is a stand-in ControlLink that records how often it was closed.
type fakeLink struct {
	mu     sync.Mutex
	closed int
}

func (l *fakeLink) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed++
	return nil
}

func newTestManager(t *testing.T) (*MemoryManager, *fakeClock, *event.Subscription) {
	t.Helper()
	clk := newFakeClock()
	bus := event.NewBus()
	sub := bus.Subscribe(64)
	t.Cleanup(sub.Close)
	m := NewMemoryManager(Options{Bus: bus, Clock: clk.Now})
	return m, clk, sub
}

func drain(sub *event.Subscription) []event.Event {
	var out []event.Event
	for {
		select {
		case ev := <-sub.Events():
			out = append(out, ev)
		default:
			return out
		}
	}
}

func TestMintShortIDsPerClientMonotonic(t *testing.T) {
	m, _, _ := newTestManager(t)
	ctx := context.Background()

	a1, err := m.Register(ctx, GatewayHello{ClientID: "cursor"}, &fakeLink{})
	if err != nil {
		t.Fatal(err)
	}
	a2, _ := m.Register(ctx, GatewayHello{ClientID: "cursor"}, &fakeLink{})
	b1, _ := m.OpenHTTP(ctx, SessionHello{ClientID: "openwebui"})

	if a1.ID != "cursor:1" || a2.ID != "cursor:2" || b1.ID != "openwebui:1" {
		t.Fatalf("IDs = %s %s %s", a1.ID, a2.ID, b1.ID)
	}
	// Seq is never reused: a re-registered gateway gets a NEW identity.
	m.Close(a2.ID)
	a3, _ := m.Register(ctx, GatewayHello{ClientID: "cursor"}, &fakeLink{})
	if a3.ID != "cursor:3" {
		t.Fatalf("re-register got %s, want cursor:3", a3.ID)
	}
}

func TestRegisterValidation(t *testing.T) {
	m, _, _ := newTestManager(t)
	ctx := context.Background()

	if _, err := m.Register(ctx, GatewayHello{ClientID: "c"}, nil); err == nil {
		t.Fatal("nil link accepted")
	}
	if _, err := m.Register(ctx, GatewayHello{}, &fakeLink{}); err == nil {
		t.Fatal("empty client ID accepted")
	}
	if _, err := m.OpenHTTP(ctx, SessionHello{}); err == nil {
		t.Fatal("empty client ID accepted for HTTP")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := m.Register(canceled, GatewayHello{ClientID: "c"}, &fakeLink{}); err == nil {
		t.Fatal("canceled ctx accepted")
	}
}

func TestHTTPTokenProtocolSide(t *testing.T) {
	m, _, _ := newTestManager(t)
	ctx := context.Background()

	h, err := m.OpenHTTP(ctx, SessionHello{ClientID: "web"})
	if err != nil {
		t.Fatal(err)
	}
	tok := h.TokenHex()
	if len(tok) != 32 {
		t.Fatalf("token hex length = %d, want 32 (128-bit)", len(tok))
	}
	if !h.MatchToken(tok) {
		t.Fatal("own token rejected")
	}
	for _, bad := range []string{"", "zz", strings.Repeat("0", 32), tok + "00", tok[:30]} {
		if h.MatchToken(bad) && bad != tok {
			t.Fatalf("bad token %q accepted", bad)
		}
	}

	h2, _ := m.OpenHTTP(ctx, SessionHello{ClientID: "web"})
	if h2.TokenHex() == tok {
		t.Fatal("two sessions share a token")
	}

	if got, ok := m.FindByToken(tok); !ok || got.ID != h.ID {
		t.Fatalf("FindByToken = %v %v", got, ok)
	}
	if _, ok := m.FindByToken(strings.Repeat("f", 32)); ok {
		t.Fatal("unknown token resolved")
	}

	// stdio sessions have no protocol token at all.
	s, _ := m.Register(ctx, GatewayHello{ClientID: "cli"}, &fakeLink{})
	if s.TokenHex() != "" || s.MatchToken(strings.Repeat("0", 32)) {
		t.Fatal("stdio session leaked a token surface")
	}
}

func TestGetListInfo(t *testing.T) {
	m, _, _ := newTestManager(t)
	ctx := context.Background()
	s, _ := m.Register(ctx, GatewayHello{ClientID: "b", Roots: []string{"/w"}}, &fakeLink{})
	m.OpenHTTP(ctx, SessionHello{ClientID: "a"}) //nolint:errcheck // exercised above

	if _, ok := m.Get(s.ID); !ok {
		t.Fatal("Get missed a live session")
	}
	if _, ok := m.Get("nope:1"); ok {
		t.Fatal("Get invented a session")
	}
	infos := m.List()
	if len(infos) != 2 || infos[0].ID != "a:1" || infos[1].ID != "b:1" {
		t.Fatalf("List = %+v, want sorted [a:1 b:1]", infos)
	}
	if infos[1].Origin != OriginStdioGateway || infos[1].Roots[0] != "/w" {
		t.Fatalf("info fields wrong: %+v", infos[1])
	}
}

func TestReaperTTLAndTouch(t *testing.T) {
	m, clk, sub := newTestManager(t)
	ctx := context.Background()

	stale, _ := m.OpenHTTP(ctx, SessionHello{ClientID: "web"})
	fresh, _ := m.OpenHTTP(ctx, SessionHello{ClientID: "web"})
	stdio, _ := m.Register(ctx, GatewayHello{ClientID: "cc"}, &fakeLink{})
	drain(sub)

	clk.Advance(23 * time.Hour)
	m.Touch(fresh.ID) // refresh: LastSeen moves to t0+23h
	if got := m.reap(clk.Now()); got != 0 {
		t.Fatalf("reap at 23h closed %d sessions", got)
	}

	clk.Advance(2 * time.Hour) // t0+25h: stale idle 25h, fresh idle 2h
	if got := m.reap(clk.Now()); got != 1 {
		t.Fatalf("reap closed %d, want 1", got)
	}
	if _, ok := m.Get(stale.ID); ok {
		t.Fatal("stale HTTP session survived TTL")
	}
	if _, ok := m.Get(fresh.ID); !ok {
		t.Fatal("touched session was reaped")
	}
	// stdio sessions are NEVER reaped: link disconnect cleans them up.
	if _, ok := m.Get(stdio.ID); !ok {
		t.Fatal("stdio session was reaped")
	}

	evs := drain(sub)
	if len(evs) != 1 || evs[0].Topic != TopicClosed {
		t.Fatalf("events = %+v", evs)
	}
	if p := evs[0].Payload.(Closed); p.Reason != ReasonExpired || p.Info.ID != stale.ID {
		t.Fatalf("closed payload = %+v", p)
	}
}

func TestRunReapsInBackground(t *testing.T) {
	clk := newFakeClock()
	bus := event.NewBus()
	sub := bus.Subscribe(8, TopicClosed)
	defer sub.Close()
	m := NewMemoryManager(Options{
		Bus: bus, Clock: clk.Now,
		HTTPTTL: time.Hour, ReapInterval: time.Millisecond,
	})
	h, _ := m.OpenHTTP(context.Background(), SessionHello{ClientID: "web"})
	clk.Advance(2 * time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	select {
	case ev := <-sub.Events():
		if p := ev.Payload.(Closed); p.Info.ID != h.ID || p.Reason != ReasonExpired {
			t.Fatalf("payload = %+v", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("background reaper never fired")
	}
}

func TestCloseCascadesAndIsIdempotent(t *testing.T) {
	m, _, sub := newTestManager(t)
	ctx := context.Background()
	link := &fakeLink{}
	s, _ := m.Register(ctx, GatewayHello{ClientID: "cc"}, link)
	drain(sub)

	m.Close(s.ID)
	m.Close(s.ID) // idempotent

	if _, ok := m.Get(s.ID); ok {
		t.Fatal("closed session still listed")
	}
	if link.closed != 1 {
		t.Fatalf("link.Close called %d times, want 1", link.closed)
	}
	evs := drain(sub)
	if len(evs) != 1 || evs[0].Topic != TopicClosed {
		t.Fatalf("events = %+v, want single closed event", evs)
	}
	if p := evs[0].Payload.(Closed); p.Reason != ReasonClosed {
		t.Fatalf("reason = %s", p.Reason)
	}
}

func TestSetRootsIsAttributeNotIdentity(t *testing.T) {
	m, _, _ := newTestManager(t)
	s, _ := m.Register(context.Background(), GatewayHello{ClientID: "cc", Roots: []string{"/a"}}, &fakeLink{})
	s.SetRoots([]string{"/b"})
	if s.ID != "cc:1" {
		t.Fatal("root change moved session identity")
	}
	got := s.Roots()
	if len(got) != 1 || got[0] != "/b" {
		t.Fatalf("roots = %v", got)
	}
	got[0] = "MUT" // returned slice is a copy
	if s.Roots()[0] != "/b" {
		t.Fatal("Roots returned an aliased slice")
	}
}
