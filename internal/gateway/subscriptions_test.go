package gateway

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// replyRecorder is a gateway whose upstream writes land in a buffer. It goes
// through the REAL FrameWriter and the real g.reply, so what a test reads
// back is what a client would have received, parsed by the same decoder.
type replyRecorder struct {
	*gateway
	buf *bytes.Buffer
}

func newReplyRecorder(t *testing.T) *replyRecorder {
	t.Helper()
	buf := &bytes.Buffer{}
	return &replyRecorder{
		gateway: &gateway{
			stateless: true,
			fw:        mcp.NewFrameWriter(buf),
			log:       slog.New(slog.DiscardHandler),
		},
		buf: buf,
	}
}

// written parses everything the gateway wrote upstream, in order.
func (r *replyRecorder) written(t *testing.T) []any {
	t.Helper()
	var out []any
	fr := mcp.NewFrameReader(bytes.NewReader(r.buf.Bytes()))
	for {
		line, err := fr.Next()
		if err != nil {
			return out
		}
		msg, perr := mcp.ParseMessage(line)
		if perr != nil {
			t.Fatalf("the gateway wrote a frame that does not parse: %v (%s)", perr, line)
		}
		out = append(out, msg)
	}
}

// TestUnsubscribedSessionsKeepGettingNotifications is the deviation, pinned.
//
// A conformant 2026-07-28 server sends nothing a client did not subscribe to.
// This one does, and the reason is in subscriptions.go: withholding leaves a
// client that never calls subscriptions/listen holding a tool set that can go
// stale forever with nothing saying so — the exact failure the HTTP face grew
// a stream to remove. If this test is ever changed, that argument is what has
// to be answered.
func TestUnsubscribedSessionsKeepGettingNotifications(t *testing.T) {
	t.Parallel()
	for _, stateless := range []bool{false, true} {
		g := &gateway{stateless: stateless}
		if !g.mayNotify(mcp.NotificationToolsListChanged) {
			t.Errorf("stateless=%v: an unsubscribed session was refused tools/list_changed; "+
				"it would hold a stale tool set with nothing saying so", stateless)
		}
	}
}

// TestSubscribingNarrowsToTheFilter: once a client uses the mechanism, the
// filter is honoured exactly — 2026-07-28's MUST NOT, for anyone who asks.
func TestSubscribingNarrowsToTheFilter(t *testing.T) {
	t.Parallel()
	g := &gateway{stateless: true}
	g.subscribed = &mcp.SubscriptionFilter{PromptsListChanged: true}

	if g.mayNotify(mcp.NotificationToolsListChanged) {
		t.Error("tools/list_changed was allowed to a session that subscribed to prompts only")
	}
	if !g.mayNotify(mcp.NotificationPromptsListChanged) {
		t.Error("prompts/list_changed was refused to a session that subscribed to it")
	}
}

// TestMayNotifyIsAnAllowList: an unknown method is refused, so a notification
// type added later cannot leak to a client that narrowed.
func TestMayNotifyIsAnAllowList(t *testing.T) {
	t.Parallel()
	g := &gateway{stateless: true}
	g.subscribed = &mcp.SubscriptionFilter{ToolsListChanged: true}

	if g.mayNotify("some/future/notification") {
		t.Error("an unknown method passed the filter; it must be an allow list, not a deny list")
	}
}

// TestHonouredFilterDropsWhatNothingProduces: the acknowledgement is the only
// place a client learns a type will never arrive, so the filter it echoes
// must be intersected with reality rather than with the protocol's full
// vocabulary.
func TestHonouredFilterDropsWhatNothingProduces(t *testing.T) {
	t.Parallel()
	got := honouredFilter(mcp.SubscriptionFilter{
		ToolsListChanged:      true,
		PromptsListChanged:    true,
		ResourcesListChanged:  true,
		ResourceSubscriptions: []string{"file:///x"},
	})
	if !got.ToolsListChanged {
		t.Error("tools/list_changed was dropped, and it is the one type this gateway produces")
	}
	if got.PromptsListChanged || got.ResourcesListChanged {
		t.Errorf("honoured a type nothing produces: %+v", got)
	}
	if got.ResourceSubscriptions != nil {
		t.Errorf("ResourceSubscriptions = %v, want nil — this hub subscribes to no individual resource, "+
			"and nil is 'none' where [] would be 'an empty set of them'", got.ResourceSubscriptions)
	}
}

// TestListenAcknowledgesBeforeItAnswers pins the order and the shape on the
// stdio face: the acknowledgement first (a client learns what it will never
// receive before it starts waiting), then a result, so a client expecting a
// response does not hang. See handleSubscriptionsListen for why answering at
// all is a binding decision rather than a reading of the specification.
func TestListenAcknowledgesBeforeItAnswers(t *testing.T) {
	t.Parallel()
	g := newReplyRecorder(t)
	params, err := json.Marshal(mcp.SubscriptionsListenParams{
		Notifications: mcp.SubscriptionFilter{ToolsListChanged: true, PromptsListChanged: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	g.handleSubscriptionsListen(mcp.NewRequest(mcp.NewIntID(4), mcp.MethodSubscriptionsListen, params))

	out := g.written(t)
	if len(out) != 2 {
		t.Fatalf("wrote %d messages, want the acknowledgement then the result", len(out))
	}
	ackMsg, ok := out[0].(*mcp.Notification)
	if !ok {
		t.Fatalf("first message is %T, want the acknowledgement notification", out[0])
	}
	if ackMsg.Method != mcp.NotificationSubscriptionsAcknowledged {
		t.Fatalf("first message = %q, want %q", ackMsg.Method, mcp.NotificationSubscriptionsAcknowledged)
	}
	var ack mcp.SubscriptionsAcknowledgedParams
	if err := json.Unmarshal(ackMsg.Params, &ack); err != nil {
		t.Fatal(err)
	}
	if !ack.Notifications.ToolsListChanged || ack.Notifications.PromptsListChanged {
		t.Errorf("acknowledged %+v; want tools only — prompts is not produced here", ack.Notifications)
	}
	if ack.Meta == nil || ack.Meta.SubscriptionID.Key() != mcp.NewIntID(4).Key() {
		t.Error("the acknowledgement does not carry the listen request's id as subscriptionId")
	}

	res, ok := out[1].(*mcp.Response)
	if !ok {
		t.Fatalf("second message is %T, want the result", out[1])
	}
	if res.Error != nil {
		t.Fatalf("subscriptions/listen answered an error: %v", res.Error)
	}

	// And the session is now narrowed.
	if g.mayNotify(mcp.NotificationPromptsListChanged) {
		t.Error("prompts/list_changed is allowed after an acknowledgement that excluded it")
	}
	if !g.mayNotify(mcp.NotificationToolsListChanged) {
		t.Error("tools/list_changed is refused after an acknowledgement that included it")
	}
}

// TestListenRejectsUnreadableParams: fail-closed on a malformed filter rather
// than defaulting to one, since either default would be a guess about what
// the client may see.
func TestListenRejectsUnreadableParams(t *testing.T) {
	t.Parallel()
	g := newReplyRecorder(t)

	g.handleSubscriptionsListen(&mcp.Request{
		ID: mcp.NewIntID(5), Method: mcp.MethodSubscriptionsListen,
		Params: json.RawMessage(`{"notifications":"not an object"}`),
	})

	out := g.written(t)
	if len(out) != 1 {
		t.Fatalf("wrote %d messages, want one error response", len(out))
	}
	res, ok := out[0].(*mcp.Response)
	if !ok || res.Error == nil {
		t.Fatalf("got %#v, want a JSON-RPC error response", out[0])
	}
	if res.Error.Code != mcp.CodeInvalidParams {
		t.Errorf("code = %d, want CodeInvalidParams", res.Error.Code)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.subscribed != nil {
		t.Error("a malformed subscribe still narrowed the session")
	}
}
