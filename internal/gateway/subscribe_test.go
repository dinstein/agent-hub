package gateway

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// testConn builds the smallest Conn the subscription paths touch: fanout logs
// through the gateway body and nothing else here reaches it. A real Open()
// costs a downstream dial, and what these tests pin is the delivery
// discipline, not the assembly.
func testConn() *Conn {
	return &Conn{g: &gateway{log: slog.New(slog.DiscardHandler)}}
}

func notif(method, params string) *mcp.Notification {
	return mcp.NewNotification(method, json.RawMessage(params))
}

// TestSubscriptionCoalescesLatestPerMethod: two notifications of one method
// collapse to ONE delivery carrying the SECOND one's params. Losing the older
// edge is the point; losing the newer one would be the bug this delivery
// discipline exists to rule out.
func TestSubscriptionCoalescesLatestPerMethod(t *testing.T) {
	t.Parallel()
	c := testConn()
	sub, err := c.Subscribe(nil)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	c.fanout(notif(mcp.NotificationToolsListChanged, `{"seq":1}`))
	c.fanout(notif(mcp.NotificationToolsListChanged, `{"seq":2}`))

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	got, ok := sub.Next(ctx)
	if !ok {
		t.Fatal("Next reported no notification; want the coalesced one")
	}
	if string(got.Params) != `{"seq":2}` {
		t.Fatalf("coalesced delivery carried %s; want the NEWEST edge {\"seq\":2}", got.Params)
	}

	// Nothing further is owed: the two collapsed into one.
	quick, stop := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer stop()
	if n, ok := sub.Next(quick); ok {
		t.Fatalf("a second delivery arrived (%s); the two should have coalesced", n.Method)
	}
}

// TestSubscriptionKeepsDistinctMethodsInArrivalOrder: coalescing collapses a
// method onto itself and must not reorder two different ones.
func TestSubscriptionKeepsDistinctMethodsInArrivalOrder(t *testing.T) {
	t.Parallel()
	c := testConn()
	sub, err := c.Subscribe(nil)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	c.fanout(notif("first/changed", `{}`))
	c.fanout(notif("second/changed", `{}`))
	c.fanout(notif("first/changed", `{"again":true}`))

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	var order []string
	for range 2 {
		n, ok := sub.Next(ctx)
		if !ok {
			t.Fatal("Next ended early")
		}
		order = append(order, n.Method)
	}
	if order[0] != "first/changed" || order[1] != "second/changed" {
		t.Fatalf("delivery order %v; want first/changed then second/changed", order)
	}
}

// TestSubscriptionFilterIsAnAllowList: a predicate that refuses a method must
// keep it out entirely. This is the 2026-07-28 rule — the filter is an allow
// list the server MUST honour exactly — compiled down to one predicate.
func TestSubscriptionFilterIsAnAllowList(t *testing.T) {
	t.Parallel()
	c := testConn()
	sub, err := c.Subscribe(func(method string) bool { return method == "wanted/changed" })
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	c.fanout(notif("unwanted/changed", `{}`))
	c.fanout(notif("wanted/changed", `{}`))

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	n, ok := sub.Next(ctx)
	if !ok {
		t.Fatal("the allowed method was not delivered")
	}
	if n.Method != "wanted/changed" {
		t.Fatalf("delivered %q; the filter should have kept it out", n.Method)
	}
}

// TestOfferNeverBlocksOnAnIdleConsumer: the read loop calls offer, and a
// consumer that stopped reading must not be able to stall the connection every
// other session on this credential shares.
func TestOfferNeverBlocksOnAnIdleConsumer(t *testing.T) {
	t.Parallel()
	c := testConn()
	sub, err := c.Subscribe(nil)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 1000 {
			c.fanout(notif(mcp.NotificationToolsListChanged, `{}`))
		}
	}()
	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("fanout blocked on a consumer that never read; the read loop would be stalled")
	}
}

// TestFanoutReachesEverySubscriber: one credential's Conn is shared by every
// session that credential opened, so a notification is owed to all of them.
func TestFanoutReachesEverySubscriber(t *testing.T) {
	t.Parallel()
	c := testConn()
	subs := make([]*Subscription, 3)
	for i := range subs {
		s, err := c.Subscribe(nil)
		if err != nil {
			t.Fatalf("Subscribe %d: %v", i, err)
		}
		defer s.Close()
		subs[i] = s
	}

	c.fanout(notif(mcp.NotificationToolsListChanged, `{}`))

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	for i, s := range subs {
		if _, ok := s.Next(ctx); !ok {
			t.Fatalf("subscriber %d received nothing", i)
		}
	}
}

// TestCloseFlushesWhatWasAlreadyPending: a closing stream hands over what it
// holds before reporting the close. The client is usually still reading the
// response body while the gateway tears down, and a last tools/list_changed
// discarded there is exactly the stale catalog this face exists to prevent.
func TestCloseFlushesWhatWasAlreadyPending(t *testing.T) {
	t.Parallel()
	c := testConn()
	sub, err := c.Subscribe(nil)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	c.fanout(notif(mcp.NotificationToolsListChanged, `{}`))
	sub.Close()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if _, ok := sub.Next(ctx); !ok {
		t.Fatal("Close discarded a notification that had already been offered")
	}
	if _, ok := sub.Next(ctx); ok {
		t.Fatal("a closed subscription kept delivering")
	}
}

// TestCloseIsIdempotentAndUnblocksNext: Close races the reaper and the
// shutdown path, and a reader parked in Next must come back rather than hang.
func TestCloseIsIdempotentAndUnblocksNext(t *testing.T) {
	t.Parallel()
	c := testConn()
	sub, err := c.Subscribe(nil)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	parked := make(chan bool, 1)
	go func() {
		_, ok := sub.Next(context.Background())
		parked <- ok
	}()
	time.Sleep(20 * time.Millisecond) // let Next park

	sub.Close()
	sub.Close() // idempotent

	select {
	case ok := <-parked:
		if ok {
			t.Fatal("Next reported a notification; want the close")
		}
	case <-time.After(testTimeout):
		t.Fatal("Close did not unblock a parked Next")
	}
}

// TestSubscribeAfterCloseFails: fail-closed. A subscription minted on a dead
// connection would never deliver anything and never say so.
func TestSubscribeAfterCloseFails(t *testing.T) {
	t.Parallel()
	c := testConn()
	c.closed = true
	if _, err := c.Subscribe(nil); err == nil {
		t.Fatal("Subscribe succeeded on a closed conn; want ErrConnClosed")
	}
}

// TestClosedSubscriptionLeavesTheConn: an abandoned entry would hold its slot
// for the life of the credential's connection.
func TestClosedSubscriptionLeavesTheConn(t *testing.T) {
	t.Parallel()
	c := testConn()
	sub, err := c.Subscribe(nil)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	sub.Close()

	c.mu.Lock()
	n := len(c.subs)
	c.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d subscriptions still registered after Close; want 0", n)
	}
}

// TestNextHonoursContextCancellation: the HTTP request's context ending is how
// a disconnected client's stream is torn down.
func TestNextHonoursContextCancellation(t *testing.T) {
	t.Parallel()
	c := testConn()
	sub, err := c.Subscribe(nil)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		if _, ok := sub.Next(ctx); ok {
			t.Error("Next reported a notification; want the cancellation")
		}
	}()
	cancel()
	select {
	case <-returned:
	case <-time.After(testTimeout):
		t.Fatal("Next ignored its cancelled context")
	}
}
