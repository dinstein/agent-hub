package httpbridge_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/httpbridge"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/tier"
)

// testSub is a Subscription a test drives by hand: push delivers, Close ends
// it. It stands in for the gateway's own, whose delivery discipline is
// pinned in internal/gateway — what these tests are about is the HTTP shape
// this face writes around it.
type testSub struct {
	ch     chan *mcp.Notification
	closed chan struct{}
	once   sync.Once
}

func newTestSub() *testSub {
	return &testSub{ch: make(chan *mcp.Notification), closed: make(chan struct{})}
}

// push hands one notification to the stream. The channel is unbuffered, so
// this returns once the handler has taken it — which is why it reports the
// timeout rather than failing the test itself: it is safe to call from the
// test goroutine, and a helper that could only fail from one is a trap for
// the next test that needs it elsewhere.
func (s *testSub) push(n *mcp.Notification) error {
	select {
	case s.ch <- n:
		return nil
	case <-time.After(5 * time.Second):
		return errors.New("the stream never read a pushed notification")
	}
}

func (s *testSub) Next(ctx context.Context) (*mcp.Notification, bool) {
	select {
	case <-ctx.Done():
		return nil, false
	case <-s.closed:
		return nil, false
	case n := <-s.ch:
		return n, true
	}
}

func (s *testSub) Close() { s.once.Do(func() { close(s.closed) }) }

// openStream mints a session, then opens the GET notification stream on it.
// It returns the response and a reader positioned at the first event.
func openStream(t *testing.T, h *harness, bearer string) (*http.Response, *bufio.Reader, string) {
	t.Helper()
	initRes := h.post(t, bearer, "", initFrame)
	if initRes.StatusCode != http.StatusOK {
		t.Fatalf("initialize status = %d, want 200", initRes.StatusCode)
	}
	session := initRes.Header.Get(httpbridge.SessionHeader)
	if session == "" {
		t.Fatal("initialize minted no session id")
	}

	req, err := http.NewRequest(http.MethodGet, h.srv.URL+httpbridge.DefaultPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set(httpbridge.SessionHeader, session)
	req.Header.Set("Accept", "text/event-stream")
	res, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	return res, bufio.NewReader(res.Body), session
}

// readEvent reads one SSE `message` event's data line, skipping comments
// (keep-alives) and blank separators.
func readEvent(t *testing.T, br *bufio.Reader) string {
	t.Helper()
	deadline := time.After(5 * time.Second)
	lines := make(chan string, 1)
	errs := make(chan error, 1)
	go func() {
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				errs <- err
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(line, "data: ") {
				lines <- strings.TrimPrefix(line, "data: ")
				return
			}
		}
	}()
	select {
	case l := <-lines:
		return l
	case err := <-errs:
		t.Fatalf("stream ended before an event arrived: %v", err)
	case <-deadline:
		t.Fatal("no event arrived on the stream")
	}
	return ""
}

// TestGetOpensTheNotificationStream is the whole point of this face: a
// notification produced behind the seam reaches a client that asked for the
// stream. Before this existed the client was told listChanged:true and then
// never heard anything.
func TestGetOpensTheNotificationStream(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	_, value := mustCreate(t, store, httpbridge.CreateSpec{Name: "streamer", Tier: tier.Write})
	sub := newTestSub()
	disp := &recordingDispatcher{
		subscribe: func(func(string) bool) (httpbridge.Subscription, error) { return sub, nil },
	}
	h := newHarness(t, &httpbridge.Authenticator{Tokens: store}, httpbridge.Options{Dispatcher: disp})

	res, br, _ := openStream(t, h, value)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	if err := sub.push(mcp.NewNotification(mcp.NotificationToolsListChanged, json.RawMessage(`{}`))); err != nil {
		t.Fatal(err)
	}
	data := readEvent(t, br)

	var got mcp.Notification
	if err := json.Unmarshal([]byte(data), &got); err != nil {
		t.Fatalf("event data is not a JSON-RPC message: %v (%s)", err, data)
	}
	if got.Method != mcp.NotificationToolsListChanged {
		t.Fatalf("delivered %q, want %q", got.Method, mcp.NotificationToolsListChanged)
	}
}

// TestGetStreamRequiresASession: the GET stream belongs to the generation
// that HAS sessions. A 2026-07-28 client has no id to present and must be
// told so rather than quietly served the older shape.
func TestGetStreamRequiresASession(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	_, value := mustCreate(t, store, httpbridge.CreateSpec{Name: "nosession", Tier: tier.Write})
	h := newHarness(t, &httpbridge.Authenticator{Tokens: store}, httpbridge.Options{})

	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+httpbridge.DefaultPath, nil)
	req.Header.Set("Authorization", "Bearer "+value)
	req.Header.Set("Accept", "text/event-stream")
	res, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (no Mcp-Session-Id)", res.StatusCode)
	}
	if n := h.disp.subscribeCount(); n != 0 {
		t.Fatalf("Subscribe reached the seam %d times for an unbound stream; want 0", n)
	}
}

// TestGetStreamNeedsCredentials: the stream is behind the same credential
// layer as everything else on this path. It is checked here rather than
// assumed, because a long-lived response is the one shape where "it worked"
// and "it hung" look alike from outside.
func TestGetStreamNeedsCredentials(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	h := newHarness(t, &httpbridge.Authenticator{Tokens: store}, httpbridge.Options{})

	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+httpbridge.DefaultPath, nil)
	req.Header.Set("Accept", "text/event-stream")
	res, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.StatusCode)
	}
}

// TestGetStreamPassesNoFilter: this generation cannot ask for a subset, so
// the seam must be handed nil — "everything" — rather than an empty allow
// list, which would mean the opposite and deliver nothing.
func TestGetStreamPassesNoFilter(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	_, value := mustCreate(t, store, httpbridge.CreateSpec{Name: "nofilter", Tier: tier.Write})
	h := newHarness(t, &httpbridge.Authenticator{Tokens: store}, httpbridge.Options{})

	openStream(t, h, value)
	waitFor(t, func() bool { return h.disp.subscribeCount() == 1 }, "Subscribe never reached the seam")
	if accept := h.disp.lastAccept(); accept != nil {
		t.Fatal("the GET stream passed a filter; this generation has no way to ask for one, so it must pass nil")
	}
}

// TestOpenStreamsDoNotConsumeTheInFlightCeiling is the load-shedding
// invariant this face had to grow a second quota for. With one in-flight slot
// configured, a parked stream must not be the thing that sheds an ordinary
// call — it is doing no work, and MaxInFlight bounds work.
func TestOpenStreamsDoNotConsumeTheInFlightCeiling(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	_, value := mustCreate(t, store, httpbridge.CreateSpec{Name: "quota", Tier: tier.Write})
	h := newHarness(t, &httpbridge.Authenticator{Tokens: store},
		httpbridge.Options{MaxInFlight: 1})

	openStream(t, h, value)
	waitFor(t, func() bool { return h.disp.subscribeCount() == 1 }, "the stream never opened")

	// The stream is parked. An ordinary call must still get through.
	res := h.post(t, value, "", initFrame)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("call status = %d while a stream was open, want 200 — the stream is holding the in-flight slot", res.StatusCode)
	}
}

// TestStreamQuotaShedsRatherThanQueues: past MaxStreams the answer is a 503,
// the same direction the in-flight ceiling takes. Queueing would make an
// exhausted quota indistinguishable from a server that simply never pushes.
func TestStreamQuotaShedsRatherThanQueues(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	_, value := mustCreate(t, store, httpbridge.CreateSpec{Name: "shed", Tier: tier.Write})
	h := newHarness(t, &httpbridge.Authenticator{Tokens: store},
		httpbridge.Options{MaxStreams: 1})

	openStream(t, h, value)
	waitFor(t, func() bool { return h.disp.subscribeCount() == 1 }, "the first stream never opened")

	initRes := h.post(t, value, "", initFrame)
	session := initRes.Header.Get(httpbridge.SessionHeader)
	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+httpbridge.DefaultPath, nil)
	req.Header.Set("Authorization", "Bearer "+value)
	req.Header.Set(httpbridge.SessionHeader, session)
	req.Header.Set("Accept", "text/event-stream")
	res, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("second stream status = %d, want 503", res.StatusCode)
	}
	if code := errorCode(t, res); code != httpbridge.CodeOverloaded {
		t.Errorf("code = %q, want %q", code, httpbridge.CodeOverloaded)
	}
}

// TestStreamClosesWhenTheSubscriptionEnds: the gateway going away must end
// the response rather than leave the client holding an open body that will
// never carry anything.
func TestStreamClosesWhenTheSubscriptionEnds(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	_, value := mustCreate(t, store, httpbridge.CreateSpec{Name: "ending", Tier: tier.Write})
	sub := newTestSub()
	disp := &recordingDispatcher{
		subscribe: func(func(string) bool) (httpbridge.Subscription, error) { return sub, nil },
	}
	h := newHarness(t, &httpbridge.Authenticator{Tokens: store}, httpbridge.Options{Dispatcher: disp})

	_, br, _ := openStream(t, h, value)
	sub.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = br.ReadString('\n') // returns once the body ends
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the response body stayed open after the subscription ended")
	}
}

// TestStreamSetupFailureIsShed: a Subscribe that fails must answer, not hang.
// The detail stays in the log — the message crosses an authenticated but
// untrusted boundary.
func TestStreamSetupFailureIsShed(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	_, value := mustCreate(t, store, httpbridge.CreateSpec{Name: "broken", Tier: tier.Write})
	disp := &recordingDispatcher{
		subscribe: func(func(string) bool) (httpbridge.Subscription, error) {
			return nil, context.DeadlineExceeded
		},
	}
	h := newHarness(t, &httpbridge.Authenticator{Tokens: store}, httpbridge.Options{Dispatcher: disp})

	initRes := h.post(t, value, "", initFrame)
	session := initRes.Header.Get(httpbridge.SessionHeader)
	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+httpbridge.DefaultPath, nil)
	req.Header.Set("Authorization", "Bearer "+value)
	req.Header.Set(httpbridge.SessionHeader, session)
	req.Header.Set("Accept", "text/event-stream")
	res, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.StatusCode)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), context.DeadlineExceeded.Error()) {
		t.Fatalf("the rejection body leaked the internal failure: %s", body)
	}
}

// waitFor polls until cond holds, failing with msg if it never does.
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}
