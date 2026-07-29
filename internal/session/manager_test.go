package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/event"
	"github.com/dinstein/agent-hub/internal/scope"
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

// fakeLink records overlay pushes; err (if set) fails PushOverlay.
type fakeLink struct {
	mu     sync.Mutex
	pushes []*scope.Overlay
	err    error
	closed int
}

func (l *fakeLink) PushOverlay(_ context.Context, ov *scope.Overlay) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return l.err
	}
	l.pushes = append(l.pushes, ov)
	return nil
}

func (l *fakeLink) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed++
	return nil
}

func (l *fakeLink) pushCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.pushes)
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

func TestMutateHTTPDirectAndEvents(t *testing.T) {
	m, _, sub := newTestManager(t)
	ctx := context.Background()
	h, _ := m.OpenHTTP(ctx, SessionHello{ClientID: "web"})
	drain(sub)

	if err := m.Mutate(ctx, h.ID, func(o *scope.Overlay) {
		o.Servers = []string{"github"}
	}); err != nil {
		t.Fatal(err)
	}
	ov := m.Overlay(h.ID)
	if ov == nil || ov.Version != 1 || len(ov.Servers) != 1 {
		t.Fatalf("overlay = %+v", ov)
	}

	if err := m.Mutate(ctx, h.ID, func(o *scope.Overlay) {
		o.Servers = []string{} // block-all: further narrowing
	}); err != nil {
		t.Fatal(err)
	}
	if got := m.Overlay(h.ID); got.Version != 2 {
		t.Fatalf("version = %d, want 2", got.Version)
	}
	// The previously published snapshot must be untouched (copy-on-write).
	if ov.Version != 1 || len(ov.Servers) != 1 {
		t.Fatalf("published snapshot mutated: %+v", ov)
	}

	evs := drain(sub)
	if len(evs) != 2 {
		t.Fatalf("events = %d, want 2 overlay events", len(evs))
	}
	for i, ev := range evs {
		if ev.Topic != TopicOverlay {
			t.Fatalf("topic = %s", ev.Topic)
		}
		p := ev.Payload.(OverlayChanged)
		if p.ID != h.ID || p.Version != uint64(i+1) {
			t.Fatalf("payload = %+v", p)
		}
	}

	if err := m.Mutate(ctx, "nope:9", func(*scope.Overlay) {}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestMutateStdioPushThenCommit(t *testing.T) {
	m, _, sub := newTestManager(t)
	ctx := context.Background()
	link := &fakeLink{}
	s, _ := m.Register(ctx, GatewayHello{ClientID: "cc"}, link)
	drain(sub)

	if err := m.Mutate(ctx, s.ID, func(o *scope.Overlay) {
		o.Servers = []string{"fs"}
	}); err != nil {
		t.Fatal(err)
	}
	if link.pushCount() != 1 {
		t.Fatalf("pushes = %d, want 1", link.pushCount())
	}
	if got := m.Overlay(s.ID); got == nil || got.Version != 1 {
		t.Fatalf("overlay after ack = %+v", got)
	}
	// The pushed overlay IS the committed one (authority == execution).
	if link.pushes[0] != m.Overlay(s.ID) {
		t.Fatal("pushed overlay differs from committed overlay")
	}

	// Push failure: nothing commits, no event, version unchanged.
	link.err = errors.New("gateway gone")
	err := m.Mutate(ctx, s.ID, func(o *scope.Overlay) { o.Servers = []string{} })
	if err == nil || !strings.Contains(err.Error(), "gateway gone") {
		t.Fatalf("err = %v", err)
	}
	if got := m.Overlay(s.ID); got.Version != 1 || got.Servers == nil || len(got.Servers) != 1 {
		t.Fatalf("failed push mutated daemon state: %+v", got)
	}
	if evs := drain(sub); len(evs) != 1 {
		t.Fatalf("events = %d, want only the successful one", len(evs))
	}
}

func TestMutateTightenOnlyAndHumanGrant(t *testing.T) {
	m, _, sub := newTestManager(t)
	ctx := context.Background()
	link := &fakeLink{}
	s, _ := m.Register(ctx, GatewayHello{ClientID: "cc"}, link)
	if err := m.Mutate(ctx, s.ID, func(o *scope.Overlay) {
		o.Servers = []string{"a"}
	}); err != nil {
		t.Fatal(err)
	}
	drain(sub)
	pushed := link.pushCount()

	// Loosening without a grant: rejected atomically, BEFORE any push.
	err := m.Mutate(ctx, s.ID, func(o *scope.Overlay) {
		o.Servers = []string{"a", "b"}
	})
	if !errors.Is(err, ErrLoosening) {
		t.Fatalf("err = %v, want ErrLoosening", err)
	}
	if link.pushCount() != pushed {
		t.Fatal("loosening was pushed to the gateway before rejection")
	}
	if got := m.Overlay(s.ID); got.Version != 1 {
		t.Fatalf("rejected mutation committed: %+v", got)
	}
	if evs := drain(sub); len(evs) != 0 {
		t.Fatalf("rejected mutation published %d events", len(evs))
	}

	// Same mutation WITH the human-grant flag: applied.
	if err := m.Mutate(ctx, s.ID, func(o *scope.Overlay) {
		o.Servers = []string{"a", "b"}
	}, WithHumanGrant()); err != nil {
		t.Fatal(err)
	}
	if got := m.Overlay(s.ID); got.Version != 2 || len(got.Servers) != 2 {
		t.Fatalf("granted mutation not applied: %+v", got)
	}
}

func TestMutateConcurrentSerialized(t *testing.T) {
	m, _, _ := newTestManager(t)
	ctx := context.Background()
	h, _ := m.OpenHTTP(ctx, SessionHello{ClientID: "web"})

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := m.Mutate(ctx, h.ID, func(o *scope.Overlay) {
				if o.Tools == nil {
					o.Tools = make(map[string]*scope.ToolSelector)
				}
				sel := o.Tools["s"]
				if sel == nil {
					sel = &scope.ToolSelector{}
					o.Tools["s"] = sel
				}
				sel.Deny = append(sel.Deny, fmt.Sprintf("tool-%d", i)) // deny grows: always a tighten
			})
			if err != nil {
				t.Errorf("mutate %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	ov := m.Overlay(h.ID)
	if ov.Version != n {
		t.Fatalf("version = %d, want %d (no lost updates)", ov.Version, n)
	}
	if got := len(ov.Tools["s"].Deny); got != n {
		t.Fatalf("denies = %d, want %d", got, n)
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
	if err := m.Mutate(ctx, s.ID, func(o *scope.Overlay) { o.Servers = []string{"a"} }); err != nil {
		t.Fatal(err)
	}
	drain(sub)

	m.Close(s.ID)
	m.Close(s.ID) // idempotent

	if _, ok := m.Get(s.ID); ok {
		t.Fatal("closed session still listed")
	}
	if s.Overlay() != nil {
		t.Fatal("overlay survived close")
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

// TestResolverIntegration wires the manager into scope.Sources the way the
// daemon will: Overlay(id) + OverlayVersion as cache key component.
func TestResolverIntegration(t *testing.T) {
	m, _, _ := newTestManager(t)
	h, _ := m.OpenHTTP(context.Background(), SessionHello{ClientID: "web"})

	var src scope.Sources
	src.Overlay = m.Overlay // signature must keep matching scope.Sources
	if ov := src.Overlay(h.ID); ov != nil {
		t.Fatalf("fresh session overlay = %+v, want nil", ov)
	}
	if err := m.Mutate(context.Background(), h.ID, func(o *scope.Overlay) {
		o.Servers = []string{"x"}
	}); err != nil {
		t.Fatal(err)
	}
	if ov := src.Overlay(h.ID); ov == nil || ov.Version != 1 {
		t.Fatalf("overlay via Sources = %+v", ov)
	}
}
