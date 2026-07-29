package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// legacyServer is a fake 2024-11-05 / 2025-03-26 HTTP+SSE server: one GET
// stream carrying every server→client message, one POST address handed out
// by the first event.
//
// DEPRECATED-UPSTREAM(http+sse, earliest-removal: deprecated 2025-03-26)
type legacyServer struct {
	*fakeServer
	events chan any // pushed onto the SSE stream
	posts  chan any // received from the client
	kill   chan struct{}
}

// newLegacyServer builds the fake. endpointData is what the endpoint event
// carries (tests use it to exercise relative, absolute and hostile forms).
func newLegacyServer(t *testing.T, endpointData string) *legacyServer {
	t.Helper()
	ls := &legacyServer{
		events: make(chan any, 8),
		posts:  make(chan any, 8),
		kill:   make(chan struct{}),
	}
	ls.fakeServer = newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			s := startSSE(t, w)
			if endpointData != "" {
				s.event("endpoint", "", []byte(endpointData))
			}
			for {
				select {
				case msg := <-ls.events:
					s.message("", msg)
				case <-ls.kill:
					return // stream dies under the client's feet
				case <-r.Context().Done():
					return
				}
			}
		}
		msg := readRPC(t, r)
		select {
		case ls.posts <- msg:
		default:
			t.Errorf("posts channel full, dropped %T", msg)
		}
		w.WriteHeader(http.StatusAccepted)
	})
	return ls
}

func dialLegacy(t *testing.T, ls *legacyServer, mutate func(*HTTPConfig)) Transport {
	t.Helper()
	cfg := HTTPConfig{URL: ls.URL + "/sse"}
	if mutate != nil {
		mutate(&cfg)
	}
	tr, err := DialHTTPSSE(testCtx(t), cfg)
	if err != nil {
		t.Fatalf("DialHTTPSSE: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	return tr
}

// nextPost returns the next message the client POSTed.
func (ls *legacyServer) nextPost(t *testing.T) any {
	t.Helper()
	select {
	case msg := <-ls.posts:
		return msg
	case <-time.After(5 * time.Second):
		t.Fatal("client posted nothing")
		return nil
	}
}

// TestHTTPSSEEndpointHandshake covers the defining move of the legacy
// binding: the POST address comes from the first SSE event, and a relative
// one resolves against the stream URL.
func TestHTTPSSEEndpointHandshake(t *testing.T) {
	ls := newLegacyServer(t, "/messages?sid=42")
	tr := dialLegacy(t, ls, nil)

	go func() {
		msg := ls.nextPost(t)
		req, ok := msg.(*mcp.Request)
		if !ok {
			t.Errorf("posted %T, want *mcp.Request", msg)
			return
		}
		ls.events <- mcp.NewResponse(req.ID, json.RawMessage(`{"tools":[]}`))
	}()

	raw, err := tr.Call(testCtx(t), mcp.MethodToolsList, mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if string(raw) != `{"tools":[]}` {
		t.Fatalf("result = %s", raw)
	}

	// The POST went to the server-supplied path, and carried no
	// MCP-Protocol-Version header (introduced after this binding was
	// already deprecated).
	var posted *recordedRequest
	for _, rec := range ls.recorded() {
		if rec.Method == http.MethodPost {
			r := rec
			posted = &r
		}
	}
	if posted == nil {
		t.Fatal("no POST recorded")
	}
	if posted.Path != "/messages?sid=42" {
		t.Fatalf("POST path = %q, want the endpoint event's", posted.Path)
	}
	if v, ok := posted.Header[http.CanonicalHeaderKey(headerProtocolVersion)]; ok {
		t.Fatalf("legacy POST carried %s: %q", headerProtocolVersion, v)
	}
	if got := posted.Header["Content-Type"]; got != mediaJSON {
		t.Fatalf("Content-Type = %q", got)
	}
}

// TestHTTPSSEEndpointCrossOrigin is the fail-closed case: the endpoint
// event is server-controlled and the caller's Authorization rides on every
// POST, so an off-origin address must be refused, not followed.
func TestHTTPSSEEndpointCrossOrigin(t *testing.T) {
	ls := newLegacyServer(t, "http://evil.invalid:9/messages")
	_, err := DialHTTPSSE(testCtx(t), HTTPConfig{
		URL:    ls.URL + "/sse",
		Header: http.Header{"Authorization": []string{"Bearer secret"}},
	})
	if !errors.Is(err, ErrHTTPProtocol) {
		t.Fatalf("err = %v, want ErrHTTPProtocol", err)
	}
	if !strings.Contains(err.Error(), "not the stream origin") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("error leaked the credential: %v", err)
	}
}

func TestHTTPSSEHandshakeFailures(t *testing.T) {
	t.Run("no endpoint event before the deadline", func(t *testing.T) {
		ls := newLegacyServer(t, "") // stream opens, says nothing
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		_, err := DialHTTPSSE(ctx, HTTPConfig{URL: ls.URL + "/sse"})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want DeadlineExceeded", err)
		}
	})
	t.Run("wrong content type", func(t *testing.T) {
		fs := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusOK)
		})
		_, err := DialHTTPSSE(testCtx(t), HTTPConfig{URL: fs.URL + "/sse"})
		if !errors.Is(err, ErrHTTPProtocol) {
			t.Fatalf("err = %v, want ErrHTTPProtocol", err)
		}
	})
	t.Run("non-200 status is classified", func(t *testing.T) {
		fs := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusGone)
		})
		_, err := DialHTTPSSE(testCtx(t), HTTPConfig{URL: fs.URL + "/sse"})
		if !errors.Is(err, ErrEndpointMoved) {
			t.Fatalf("err = %v, want ErrEndpointMoved", err)
		}
	})
	t.Run("unparsable endpoint event", func(t *testing.T) {
		ls := newLegacyServer(t, "://not a url")
		_, err := DialHTTPSSE(testCtx(t), HTTPConfig{URL: ls.URL + "/sse"})
		if !errors.Is(err, ErrHTTPProtocol) {
			t.Fatalf("err = %v, want ErrHTTPProtocol", err)
		}
	})
}

// TestHTTPSSEReverseRPC covers a server-initiated request arriving on the
// stream and answered by POST — the reply travels on the other channel.
func TestHTTPSSEReverseRPC(t *testing.T) {
	ls := newLegacyServer(t, "/messages")
	tr := dialLegacy(t, ls, nil)
	tr.OnPeerRequest(func(_ context.Context, req *mcp.Request) (*mcp.Response, error) {
		out, _ := json.Marshal(mcp.ListRootsResult{Roots: []mcp.Root{{URI: "file:///w"}}})
		return mcp.NewResponse(mcp.NewIntID(1234), out), nil // id must be forced
	})

	ls.events <- mcp.NewRequest(mcp.NewStringID("srv-9"), mcp.MethodRootsList, nil)

	msg := ls.nextPost(t)
	resp, ok := msg.(*mcp.Response)
	if !ok {
		t.Fatalf("posted %T, want *mcp.Response", msg)
	}
	if resp.ID.Key() != mcp.NewStringID("srv-9").Key() {
		t.Fatalf("peer reply id = %s, want srv-9", resp.ID)
	}
	var roots mcp.ListRootsResult
	if err := json.Unmarshal(resp.Result, &roots); err != nil || len(roots.Roots) != 1 {
		t.Fatalf("peer reply result = %s (%v)", resp.Result, err)
	}
}

func TestHTTPSSEListChanged(t *testing.T) {
	ls := newLegacyServer(t, "/messages")
	tr := dialLegacy(t, ls, nil)
	changed := make(chan ChangeMask, 4)
	tr.OnListChanged(func(m ChangeMask) { changed <- m })

	ls.events <- mcp.NewNotification(mcp.NotificationToolsListChanged, nil)
	ls.events <- mcp.NewNotification("notifications/unknown/thing", nil)
	ls.events <- mcp.NewNotification(mcp.NotificationPromptsListChanged, nil)

	got := make([]ChangeMask, 0, 2)
	for len(got) < 2 {
		select {
		case m := <-changed:
			got = append(got, m)
		case <-time.After(5 * time.Second):
			t.Fatalf("only got %v", got)
		}
	}
	if got[0] != ChangeTools || got[1] != ChangePrompts {
		t.Fatalf("masks = %v (unknown notifications must be ignored, not forwarded)", got)
	}
}

// TestHTTPSSEStreamDeathFailsPending pins the stdio-like failure model:
// this binding has one long-lived stream, so losing it is terminal.
func TestHTTPSSEStreamDeathFailsPending(t *testing.T) {
	ls := newLegacyServer(t, "/messages")
	tr := dialLegacy(t, ls, nil)

	errCh := make(chan error, 1)
	go func() {
		_, err := tr.Call(testCtx(t), mcp.MethodToolsList, nil)
		errCh <- err
	}()

	ls.nextPost(t) // the call is in flight
	close(ls.kill) // the stream dies without answering

	select {
	case err := <-errCh:
		te := transportError(t, err)
		if te.Class != ClassUnavailable {
			t.Fatalf("class = %s, want unavailable", te.Class)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pending call was not released when the stream died")
	}

	// The transport is terminally failed: later calls fail immediately.
	if _, err := tr.Call(testCtx(t), mcp.MethodPing, nil); err == nil {
		t.Fatal("call on a failed transport succeeded")
	}
	if err := tr.Notify(testCtx(t), mcp.NotificationInitialized, nil); err == nil {
		t.Fatal("notify on a failed transport succeeded")
	}
}

func TestHTTPSSEPostStatusClassification(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		header    http.Header
		wantClass Class
		wantRetry time.Duration
		wantErrIs error
	}{
		{name: "429", status: http.StatusTooManyRequests, header: http.Header{headerRetryAfter: []string{"3"}}, wantClass: ClassRetry, wantRetry: 3 * time.Second},
		{name: "500", status: http.StatusInternalServerError, wantClass: ClassUnavailable},
		{name: "410", status: http.StatusGone, wantClass: ClassUnavailable, wantErrIs: ErrEndpointMoved},
		{name: "400", status: http.StatusBadRequest, wantClass: ClassFatal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					s := startSSE(t, w)
					s.event("endpoint", "", []byte("/messages"))
					<-r.Context().Done()
					return
				}
				for k, vs := range tt.header {
					for _, v := range vs {
						w.Header().Add(k, v)
					}
				}
				w.WriteHeader(tt.status)
			})
			tr, err := DialHTTPSSE(testCtx(t), HTTPConfig{URL: fs.URL + "/sse"})
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer func() { _ = tr.Close() }()

			_, err = tr.Call(testCtx(t), mcp.MethodToolsList, nil)
			te := transportError(t, err)
			if te.Class != tt.wantClass {
				t.Fatalf("class = %s, want %s (err %v)", te.Class, tt.wantClass, err)
			}
			if te.RetryAfter != tt.wantRetry {
				t.Fatalf("RetryAfter = %v, want %v", te.RetryAfter, tt.wantRetry)
			}
			if tt.wantErrIs != nil && !errors.Is(err, tt.wantErrIs) {
				t.Fatalf("err = %v, want errors.Is %v", err, tt.wantErrIs)
			}
			// A POST failure is per-request: the stream is untouched, so
			// the transport stays usable (unlike a stream death).
			if _, err := tr.Call(testCtx(t), mcp.MethodPing, nil); err == nil {
				t.Fatal("second call unexpectedly succeeded")
			} else if !errors.Is(err, ErrClosed) && transportError(t, err).Class != tt.wantClass {
				t.Fatalf("second call class changed: %v", err)
			}
		})
	}
}

// TestHTTPSSEContextCancel pins cancellation: ctx.Err() surfaces and
// notifications/cancelled is POSTed naming the abandoned request.
func TestHTTPSSEContextCancel(t *testing.T) {
	ls := newLegacyServer(t, "/messages")
	tr := dialLegacy(t, ls, nil)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := tr.Call(ctx, mcp.MethodToolsCall, mcp.CallToolParams{Name: "slow"})
		errCh <- err
	}()

	req, ok := ls.nextPost(t).(*mcp.Request)
	if !ok {
		t.Fatal("first post is not a request")
	}
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled call never returned")
	}

	msg := ls.nextPost(t)
	n, ok := msg.(*mcp.Notification)
	if !ok || n.Method != mcp.NotificationCancelled {
		t.Fatalf("second post = %#v, want notifications/cancelled", msg)
	}
	var p mcp.CancelledParams
	if err := json.Unmarshal(n.Params, &p); err != nil {
		t.Fatalf("decode cancelled params: %v", err)
	}
	if p.RequestID.Key() != req.ID.Key() {
		t.Fatalf("cancelled requestId = %s, want %s", p.RequestID, req.ID)
	}
}

func TestHTTPSSECloseIsIdempotent(t *testing.T) {
	ls := newLegacyServer(t, "/messages")
	tr := dialLegacy(t, ls, nil)

	if tr.Stderr() != "" {
		t.Fatalf("Stderr = %q, want empty", tr.Stderr())
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := tr.Call(testCtx(t), mcp.MethodPing, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
}

// TestHTTPSSEMalformedMessageIsTerminal pins that a peer emitting garbage
// cannot keep framing intact, so the transport fails rather than guessing.
func TestHTTPSSEMalformedMessageIsTerminal(t *testing.T) {
	kill := make(chan struct{})
	fs := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		s := startSSE(t, w)
		s.event("endpoint", "", []byte("/messages"))
		<-kill
		s.event("message", "", []byte(`{"jsonrpc":"9.9","id":1}`))
		<-r.Context().Done()
	})
	tr, err := DialHTTPSSE(testCtx(t), HTTPConfig{URL: fs.URL + "/sse"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = tr.Close() }()

	errCh := make(chan error, 1)
	go func() {
		_, err := tr.Call(testCtx(t), mcp.MethodToolsList, nil)
		errCh <- err
	}()
	time.Sleep(50 * time.Millisecond)
	close(kill)

	select {
	case err := <-errCh:
		if !errors.Is(err, mcp.ErrMalformedFrame) {
			t.Fatalf("err = %v, want ErrMalformedFrame", err)
		}
		if te := transportError(t, err); te.Class != ClassUnavailable {
			t.Fatalf("class = %s, want unavailable", te.Class)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("malformed frame did not release the pending call")
	}
}

// TestHTTPSSEOversizedEvent pins the bounded read on the legacy stream.
func TestHTTPSSEOversizedEvent(t *testing.T) {
	const limit = 2048
	kill := make(chan struct{})
	fs := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		s := startSSE(t, w)
		s.event("endpoint", "", []byte("/messages"))
		<-kill
		s.event("message", "", []byte(`{"jsonrpc":"2.0","id":1,"result":{"blob":"`+strings.Repeat("x", limit*2)+`"}}`))
		<-r.Context().Done()
	})
	tr, err := DialHTTPSSE(testCtx(t), HTTPConfig{URL: fs.URL + "/sse", MaxFrame: limit})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = tr.Close() }()

	errCh := make(chan error, 1)
	go func() {
		_, err := tr.Call(testCtx(t), mcp.MethodToolsList, nil)
		errCh <- err
	}()
	time.Sleep(50 * time.Millisecond)
	close(kill)

	select {
	case err := <-errCh:
		if !errors.Is(err, mcp.ErrFrameTooLarge) {
			t.Fatalf("err = %v, want ErrFrameTooLarge", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("oversized event did not release the pending call")
	}
}
