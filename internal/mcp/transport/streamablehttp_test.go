package transport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// dialStreamable is the test constructor: it returns the concrete type so
// tests can reach internal state (session id) the interface hides.
func dialStreamable(t *testing.T, cfg HTTPConfig) *streamableHTTP {
	t.Helper()
	tr, err := DialStreamableHTTP(cfg)
	if err != nil {
		t.Fatalf("DialStreamableHTTP: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	sh, ok := tr.(*streamableHTTP)
	if !ok {
		t.Fatalf("transport is %T", tr)
	}
	return sh
}

func initResult(t *testing.T, version string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(mcp.InitializeResult{
		ProtocolVersion: version,
		Capabilities:    json.RawMessage(`{"tools":{"listChanged":true}}`),
		ServerInfo:      mcp.Implementation{Name: "fake-http", Version: "1"},
	})
	if err != nil {
		t.Fatalf("marshal initialize result: %v", err)
	}
	return raw
}

// TestStreamableHTTPSingleJSONResponse covers the plain application/json
// answer plus the Mcp-Session-Id round trip and the negotiated
// MCP-Protocol-Version header.
func TestStreamableHTTPSingleJSONResponse(t *testing.T) {
	const sessionID = "sess-abc-123"
	fs := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete { // Close's session teardown
			w.WriteHeader(http.StatusNoContent)
			return
		}
		msg := readRPC(t, r)
		switch m := msg.(type) {
		case *mcp.Request:
			if m.Method == mcp.MethodInitialize {
				if got := r.Header.Get(headerSessionID); got != "" {
					t.Errorf("initialize carried a session id %q", got)
				}
				if got := r.Header.Get(headerProtocolVersion); got != mcp.ProtocolVersion {
					t.Errorf("initialize protocol header = %q, want %q", got, mcp.ProtocolVersion)
				}
				w.Header().Set(headerSessionID, sessionID)
				writeJSONRPC(t, w, mcp.NewResponse(m.ID, initResult(t, "2025-06-18")))
				return
			}
			// Every later request must echo the session and the negotiated
			// (downgraded) protocol version.
			if got := r.Header.Get(headerSessionID); got != sessionID {
				t.Errorf("%s session header = %q, want %q", m.Method, got, sessionID)
			}
			if got := r.Header.Get(headerProtocolVersion); got != "2025-06-18" {
				t.Errorf("%s protocol header = %q, want negotiated 2025-06-18", m.Method, got)
			}
			if !strings.Contains(r.Header.Get(headerAccept), mediaSSE) {
				t.Errorf("Accept = %q, want it to allow %s", r.Header.Get(headerAccept), mediaSSE)
			}
			writeJSONRPC(t, w, mcp.NewResponse(m.ID, json.RawMessage(`{"tools":[]}`)))
		case *mcp.Notification:
			if got := r.Header.Get(headerSessionID); got != sessionID {
				t.Errorf("notification session header = %q, want %q", got, sessionID)
			}
			w.WriteHeader(http.StatusAccepted)
		default:
			t.Errorf("unexpected message %T", msg)
			w.WriteHeader(http.StatusBadRequest)
		}
	})

	tr := dialStreamable(t, HTTPConfig{URL: fs.URL + "/mcp"})

	// Version negotiation goes through the shared initialize.go path.
	res, err := initializeLegacy(testCtx(t), tr, mcp.Implementation{Name: "agenthub", Version: "test"})
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if res.ProtocolVersion != "2025-06-18" {
		t.Fatalf("negotiated %q, want the server's 2025-06-18", res.ProtocolVersion)
	}
	tr.mu.Lock()
	gotSession := tr.sessionID
	tr.mu.Unlock()
	if gotSession != sessionID {
		t.Fatalf("session id = %q, want %q", gotSession, sessionID)
	}

	raw, err := tr.Call(testCtx(t), mcp.MethodToolsList, mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if string(raw) != `{"tools":[]}` {
		t.Fatalf("result = %s", raw)
	}
}

// TestStreamableHTTP2026Handshake covers the discover-based handshake: no
// initialize round-trip, and every later POST carries the per-request _meta
// plus the Mcp-Method / Mcp-Name headers.
func TestStreamableHTTP2026Handshake(t *testing.T) {
	fs := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		msg := readRPC(t, r)
		m, ok := msg.(*mcp.Request)
		if !ok {
			t.Errorf("unexpected message %T", msg)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch m.Method {
		case mcp.MethodInitialize:
			t.Error("2026 handshake must not send initialize")
			w.WriteHeader(http.StatusBadRequest)
		case mcp.MethodDiscover:
			// The probe declares 2026-07-28 in its _meta before anything is
			// negotiated; the header MUST say the same or a conformant server
			// answers 400 -32020 and the connection never happens.
			if got := r.Header.Get(headerProtocolVersion); got != mcp.Version2026 {
				t.Errorf("discover %s = %q, want %q", headerProtocolVersion, got, mcp.Version2026)
			}
			result, _ := json.Marshal(mcp.DiscoverResult{
				ResultType:        mcp.ResultTypeComplete,
				SupportedVersions: []string{"2026-07-28"},
				Meta:              &mcp.ResultMeta{ServerInfo: &mcp.Implementation{Name: "stub2026", Version: "1"}},
			})
			writeJSONRPC(t, w, mcp.NewResponse(m.ID, result))
		case mcp.MethodToolsCall:
			if got := r.Header.Get(headerMcpMethod); got != mcp.MethodToolsCall {
				t.Errorf("Mcp-Method = %q, want %q", got, mcp.MethodToolsCall)
			}
			if got := r.Header.Get(headerMcpName); got != "echo" {
				t.Errorf("Mcp-Name = %q, want %q", got, "echo")
			}
			if got := r.Header.Get(headerProtocolVersion); got != mcp.Version2026 {
				t.Errorf("protocol header = %q, want %q", got, mcp.Version2026)
			}
			var p struct {
				Name string           `json:"name"`
				Meta *mcp.RequestMeta `json:"_meta"`
			}
			if err := json.Unmarshal(m.Params, &p); err != nil {
				t.Errorf("decode tools/call params %s: %v", m.Params, err)
			}
			if p.Meta == nil || p.Meta.ProtocolVersion != mcp.Version2026 {
				t.Errorf("tools/call _meta = %+v, want protocolVersion %q", p.Meta, mcp.Version2026)
			}
			writeJSONRPC(t, w, mcp.NewResponse(m.ID, json.RawMessage(`{"resultType":"complete","content":[]}`)))
		default:
			t.Errorf("unexpected method %q", m.Method)
			w.WriteHeader(http.StatusBadRequest)
		}
	})

	tr := dialStreamable(t, HTTPConfig{URL: fs.URL + "/mcp"})
	res, err := Handshake(testCtx(t), tr, mcp.Implementation{Name: "agenthub", Version: "test"})
	if err != nil {
		t.Fatalf("Handshake: %v", err)
	}
	if res.Version != mcp.Version2026 {
		t.Fatalf("negotiated %q, want %q", res.Version, mcp.Version2026)
	}
	// The discover POST is itself a 2026-shaped request and must carry
	// Mcp-Method even though nothing is negotiated yet — a strict stateless
	// server rejects it with -32020 otherwise, which would masquerade as
	// "old server" and silently divert every 2026 server to the legacy path.
	if got := fs.recorded()[0].Header[headerMcpMethod]; got != mcp.MethodDiscover {
		t.Fatalf("discover POST Mcp-Method = %q, want %q", got, mcp.MethodDiscover)
	}
	if _, err := tr.Call(testCtx(t), mcp.MethodToolsCall, mcp.CallToolParams{
		Name: "echo", Arguments: json.RawMessage(`{"s":"hi"}`),
	}); err != nil {
		t.Fatalf("tools/call: %v", err)
	}
}

// TestStreamableHTTPStreamResponse covers the text/event-stream answer: a
// notification arrives mid-call and the response closes the stream.
func TestStreamableHTTPStreamResponse(t *testing.T) {
	fs := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		req := readRequestRPC(t, r)
		s := startSSE(t, w)
		s.comment("keep-alive before any message")
		s.message("e1", mcp.NewNotification(mcp.NotificationToolsListChanged, nil))
		s.event("vendor-ping", "e2", []byte("not a jsonrpc message"))
		s.message("e3", mcp.NewResponse(req.ID, json.RawMessage(`{"ok":true}`)))
	})

	tr := dialStreamable(t, HTTPConfig{URL: fs.URL + "/mcp"})
	changed := make(chan ChangeMask, 4)
	tr.OnListChanged(func(m ChangeMask) { changed <- m })

	raw, err := tr.Call(testCtx(t), mcp.MethodToolsCall, mcp.CallToolParams{Name: "x"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if string(raw) != `{"ok":true}` {
		t.Fatalf("result = %s", raw)
	}
	select {
	case m := <-changed:
		if m != ChangeTools {
			t.Fatalf("mask = %s", m)
		}
	default:
		t.Fatal("list_changed notification on the response stream was dropped")
	}
	tr.mu.Lock()
	last := tr.lastEventID
	tr.mu.Unlock()
	if last != "e3" {
		t.Fatalf("lastEventID = %q, want e3", last)
	}
}

// TestStreamableHTTPReverseRPCOverStream covers a server-initiated request
// arriving on the response stream: the client answers it with a separate
// POST, and only then does the server finish the original call.
func TestStreamableHTTPReverseRPCOverStream(t *testing.T) {
	answered := make(chan *mcp.Response, 1)
	fs := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		msg := readRPC(t, r)
		switch m := msg.(type) {
		case *mcp.Response:
			// The client's answer to our reverse RPC.
			answered <- m
			w.WriteHeader(http.StatusAccepted)
		case *mcp.Request:
			s := startSSE(t, w)
			s.message("r1", mcp.NewRequest(mcp.NewStringID("srv-1"), mcp.MethodRootsList, nil))
			select {
			case peerResp := <-answered:
				var roots mcp.ListRootsResult
				if err := json.Unmarshal(peerResp.Result, &roots); err != nil {
					t.Errorf("decode roots result: %v", err)
				}
				if peerResp.ID.Key() != mcp.NewStringID("srv-1").Key() {
					t.Errorf("peer response id = %s, want the request id", peerResp.ID)
				}
				out, _ := json.Marshal(map[string]any{"roots": len(roots.Roots)})
				s.message("r2", mcp.NewResponse(m.ID, out))
			case <-time.After(5 * time.Second):
				t.Error("client never answered the reverse RPC")
			}
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	})

	tr := dialStreamable(t, HTTPConfig{URL: fs.URL + "/mcp"})
	tr.OnPeerRequest(func(_ context.Context, req *mcp.Request) (*mcp.Response, error) {
		if req.Method != mcp.MethodRootsList {
			return nil, errors.New("unexpected method " + req.Method)
		}
		out, _ := json.Marshal(mcp.ListRootsResult{Roots: []mcp.Root{{URI: "file:///w", Name: "w"}}})
		// The id is forced by the transport; a wrong one here must not leak.
		return mcp.NewResponse(mcp.NewIntID(999), out), nil
	})

	raw, err := tr.Call(testCtx(t), mcp.MethodToolsCall, mcp.CallToolParams{Name: "x"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if string(raw) != `{"roots":1}` {
		t.Fatalf("result = %s", raw)
	}
}

// TestStreamableHTTPPeerWithoutHandler pins the fallback: an unhandled
// reverse RPC gets a method-not-found answer, never silence.
func TestStreamableHTTPPeerWithoutHandler(t *testing.T) {
	answered := make(chan *mcp.Response, 1)
	fs := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		msg := readRPC(t, r)
		switch m := msg.(type) {
		case *mcp.Response:
			answered <- m
			w.WriteHeader(http.StatusAccepted)
		case *mcp.Request:
			s := startSSE(t, w)
			// DEPRECATED-UPSTREAM(sampling, earliest-removal: 2027-07-28):
			// scripts the reverse RPC a ≤ 2025-11-25 server may send.
			s.message("r1", mcp.NewRequest(mcp.NewStringID("srv-1"), "sampling/createMessage", nil))
			select {
			case resp := <-answered:
				out, _ := json.Marshal(map[string]any{"code": resp.Error.Code})
				s.message("r2", mcp.NewResponse(m.ID, out))
			case <-time.After(5 * time.Second):
				t.Error("no answer to the unhandled reverse RPC")
			}
		}
	})

	tr := dialStreamable(t, HTTPConfig{URL: fs.URL + "/mcp"})
	raw, err := tr.Call(testCtx(t), mcp.MethodToolsCall, nil)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	want := `{"code":` + strconv.Itoa(mcp.CodeMethodNotFound) + `}`
	if string(raw) != want {
		t.Fatalf("result = %s, want %s", raw, want)
	}
}

func TestStreamableHTTPStatusClassification(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		header     http.Header
		wantClass  Class
		wantRetry  time.Duration
		wantErrIs  error
		wantCalls2 int // requests the server sees after a second Call
	}{
		{
			name:       "429 with Retry-After is the only retryable server answer",
			status:     http.StatusTooManyRequests,
			header:     http.Header{headerRetryAfter: []string{"2"}},
			wantClass:  ClassRetry,
			wantRetry:  2 * time.Second,
			wantCalls2: 2,
		},
		{
			name:       "500 counts against the breaker but is never replayed",
			status:     http.StatusInternalServerError,
			wantClass:  ClassUnavailable,
			wantCalls2: 2,
		},
		{
			name:       "503 is unavailable, not retry",
			status:     http.StatusServiceUnavailable,
			wantClass:  ClassUnavailable,
			wantCalls2: 2,
		},
		{
			name:       "410 Gone poisons the endpoint forever",
			status:     http.StatusGone,
			wantClass:  ClassUnavailable,
			wantErrIs:  ErrEndpointMoved,
			wantCalls2: 1, // the second call never leaves the process
		},
		{
			name:       "404 expires the session",
			status:     http.StatusNotFound,
			wantClass:  ClassUnavailable,
			wantErrIs:  ErrSessionExpired,
			wantCalls2: 2,
		},
		{
			name:       "400 is fatal: our request was bad, the server is fine",
			status:     http.StatusBadRequest,
			wantClass:  ClassFatal,
			wantCalls2: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request) {
				for k, vs := range tt.header {
					for _, v := range vs {
						w.Header().Add(k, v)
					}
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(`{"error":"nope"}`))
			})
			tr := dialStreamable(t, HTTPConfig{URL: fs.URL + "/mcp"})
			// Seed a session id so the 404 case has one to invalidate.
			tr.mu.Lock()
			tr.sessionID = "seeded"
			tr.mu.Unlock()

			_, err := tr.Call(testCtx(t), mcp.MethodToolsList, nil)
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

			_, _ = tr.Call(testCtx(t), mcp.MethodToolsList, nil)
			if got := fs.count(); got != tt.wantCalls2 {
				t.Fatalf("server saw %d requests, want %d", got, tt.wantCalls2)
			}
			if tt.status == http.StatusNotFound {
				tr.mu.Lock()
				sid := tr.sessionID
				tr.mu.Unlock()
				if sid != "" {
					t.Fatalf("session id %q survived a 404", sid)
				}
			}
		})
	}
}

// TestStreamableHTTPGoneSkipsDelete pins that a 410 endpoint is never
// contacted again — not even by Close's session DELETE.
func TestStreamableHTTPGoneSkipsDelete(t *testing.T) {
	fs := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			t.Error("DELETE sent to an endpoint that answered 410")
		}
		w.Header().Set(headerSessionID, "s1")
		w.WriteHeader(http.StatusGone)
	})
	tr := dialStreamable(t, HTTPConfig{URL: fs.URL + "/mcp"})
	if _, err := tr.Call(testCtx(t), mcp.MethodToolsList, nil); !errors.Is(err, ErrEndpointMoved) {
		t.Fatalf("err = %v, want ErrEndpointMoved", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := fs.count(); got != 1 {
		t.Fatalf("server saw %d requests after 410, want 1", got)
	}
}

func TestStreamableHTTPFrameBounds(t *testing.T) {
	const limit = 4096
	big := strings.Repeat("x", limit*2)

	t.Run("oversized json response body", func(t *testing.T) {
		fs := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
			req := readRequestRPC(t, r)
			out, _ := json.Marshal(map[string]string{"blob": big})
			writeJSONRPC(t, w, mcp.NewResponse(req.ID, out))
		})
		tr := dialStreamable(t, HTTPConfig{URL: fs.URL + "/mcp", MaxFrame: limit})
		_, err := tr.Call(testCtx(t), mcp.MethodToolsList, nil)
		if !errors.Is(err, mcp.ErrFrameTooLarge) {
			t.Fatalf("err = %v, want ErrFrameTooLarge", err)
		}
		if te := transportError(t, err); te.Class != ClassUnavailable {
			t.Fatalf("class = %s, want unavailable", te.Class)
		}
	})

	t.Run("oversized sse event", func(t *testing.T) {
		fs := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
			req := readRequestRPC(t, r)
			s := startSSE(t, w)
			out, _ := json.Marshal(map[string]string{"blob": big})
			s.message("e1", mcp.NewResponse(req.ID, out))
		})
		tr := dialStreamable(t, HTTPConfig{URL: fs.URL + "/mcp", MaxFrame: limit})
		_, err := tr.Call(testCtx(t), mcp.MethodToolsList, nil)
		if !errors.Is(err, mcp.ErrFrameTooLarge) {
			t.Fatalf("err = %v, want ErrFrameTooLarge", err)
		}
	})

	t.Run("oversized outgoing request never leaves the process", func(t *testing.T) {
		fs := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request) {
			t.Error("oversized request was sent")
			w.WriteHeader(http.StatusOK)
		})
		tr := dialStreamable(t, HTTPConfig{URL: fs.URL + "/mcp", MaxFrame: limit})
		_, err := tr.Call(testCtx(t), mcp.MethodToolsCall, map[string]string{"blob": big})
		if !errors.Is(err, mcp.ErrFrameTooLarge) {
			t.Fatalf("err = %v, want ErrFrameTooLarge", err)
		}
		if te := transportError(t, err); te.Class != ClassFatal {
			t.Fatalf("class = %s, want fatal (retrying the same payload cannot help)", te.Class)
		}
		if fs.count() != 0 {
			t.Fatalf("server saw %d requests", fs.count())
		}
	})
}

// TestStreamableHTTPContextCancel pins the cancellation contract: the call
// returns ctx.Err() (not a transport error) and notifications/cancelled is
// forwarded best-effort naming the abandoned request id.
func TestStreamableHTTPContextCancel(t *testing.T) {
	cancelled := make(chan mcp.CancelledParams, 1)
	streamOpen := make(chan struct{})
	fs := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		msg := readRPC(t, r)
		switch m := msg.(type) {
		case *mcp.Notification:
			if m.Method == mcp.NotificationCancelled {
				var p mcp.CancelledParams
				if err := json.Unmarshal(m.Params, &p); err != nil {
					t.Errorf("decode cancelled params: %v", err)
				}
				select {
				case cancelled <- p:
				default:
				}
			}
			w.WriteHeader(http.StatusAccepted)
		case *mcp.Request:
			s := startSSE(t, w)
			s.comment("open, but never answering")
			close(streamOpen)
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}
	})

	tr := dialStreamable(t, HTTPConfig{URL: fs.URL + "/mcp"})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-streamOpen
		cancel()
	}()

	_, err := tr.Call(ctx, mcp.MethodToolsCall, mcp.CallToolParams{Name: "slow"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	select {
	case p := <-cancelled:
		if p.RequestID.Key() != mcp.NewIntID(1).Key() {
			t.Fatalf("cancelled requestId = %s, want 1", p.RequestID)
		}
		if p.Reason == "" {
			t.Fatal("cancelled reason is empty")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("notifications/cancelled was never forwarded")
	}
}

// TestStreamableHTTPNotificationStream covers the optional GET stream: it
// carries notifications that belong to no call, and it reconnects.
func TestStreamableHTTPNotificationStream(t *testing.T) {
	gets := make(chan string, 4) // Last-Event-ID of each GET
	fs := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodGet {
			gets <- r.Header.Get(headerLastEventID)
			s := startSSE(t, w)
			s.message("n1", mcp.NewNotification(mcp.NotificationResourcesListChanged, nil))
			return // stream ends; the client must reconnect
		}
		req := readRequestRPC(t, r)
		w.Header().Set(headerSessionID, "s9")
		writeJSONRPC(t, w, mcp.NewResponse(req.ID, initResult(t, mcp.ProtocolVersion)))
	})

	tr := dialStreamable(t, HTTPConfig{
		URL:                fs.URL + "/mcp",
		NotificationStream: true,
		retryBase:          time.Millisecond,
	})
	changed := make(chan ChangeMask, 8)
	tr.OnListChanged(func(m ChangeMask) { changed <- m })

	if _, err := tr.Call(testCtx(t), mcp.MethodInitialize, mcp.InitializeParams{ProtocolVersion: mcp.ProtocolVersion}); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	select {
	case m := <-changed:
		if m != ChangeResources {
			t.Fatalf("mask = %s", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("notification stream delivered nothing")
	}
	// The first GET carries no Last-Event-ID; the reconnect carries the
	// last id seen.
	if got := <-gets; got != "" {
		t.Fatalf("first GET Last-Event-ID = %q, want empty", got)
	}
	select {
	case got := <-gets:
		if got != "n1" {
			t.Fatalf("reconnect Last-Event-ID = %q, want n1", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("notification stream never reconnected")
	}
}

// TestStreamableHTTP2026SubscriptionsListen covers the 2026 replacement for
// the GET notification stream: after a discover handshake the transport
// POSTs subscriptions/listen (with _meta and the opted-in event types), and
// notifications on the long-lived SSE answer dispatch exactly like the GET
// stream's did — including reconnecting when the stream ends.
func TestStreamableHTTP2026SubscriptionsListen(t *testing.T) {
	listens := make(chan mcp.SubscriptionsListenParams, 4)
	fs := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		m := readRequestRPC(t, r)
		switch m.Method {
		case mcp.MethodDiscover:
			result, _ := json.Marshal(mcp.DiscoverResult{
				SupportedVersions: []string{mcp.Version2026},
				Meta:              &mcp.ResultMeta{ServerInfo: &mcp.Implementation{Name: "stub2026", Version: "1"}},
			})
			writeJSONRPC(t, w, mcp.NewResponse(m.ID, result))
		case mcp.MethodSubscriptionsListen:
			if got := r.Header.Get(headerMcpMethod); got != mcp.MethodSubscriptionsListen {
				t.Errorf("listen Mcp-Method = %q", got)
			}
			var p mcp.SubscriptionsListenParams
			if err := json.Unmarshal(m.Params, &p); err != nil {
				t.Errorf("decode listen params: %v", err)
			}
			// Decode the wire shape too, not only our own struct: a
			// round-trip through the type under test agrees with itself no
			// matter what member name it picked.
			var wire struct {
				Notifications map[string]any `json:"notifications"`
			}
			if err := json.Unmarshal(m.Params, &wire); err != nil {
				t.Errorf("decode listen params as the spec shape: %v", err)
			} else if wire.Notifications["toolsListChanged"] != true {
				t.Errorf("params.notifications = %v, want an object opting into toolsListChanged", wire.Notifications)
			}
			listens <- p
			s := startSSE(t, w)
			// The acknowledgment is the first message a conformant server
			// sends, and it declines promptsListChanged here: the client must
			// carry on with what it did get.
			ack, _ := json.Marshal(mcp.SubscriptionsAcknowledgedParams{
				Notifications: mcp.SubscriptionFilter{ToolsListChanged: true, ResourcesListChanged: true},
				Meta:          &mcp.NotificationMeta{SubscriptionID: m.ID},
			})
			s.message("", mcp.NewNotification(mcp.NotificationSubscriptionsAcknowledged, ack))
			s.message("", mcp.NewNotification(mcp.NotificationToolsListChanged, nil))
			// stream ends; the client must re-POST
		default:
			t.Errorf("unexpected method %q", m.Method)
			w.WriteHeader(http.StatusBadRequest)
		}
	})

	tr := dialStreamable(t, HTTPConfig{
		URL:                fs.URL + "/mcp",
		NotificationStream: true,
		retryBase:          time.Millisecond,
	})
	changed := make(chan ChangeMask, 8)
	tr.OnListChanged(func(m ChangeMask) { changed <- m })

	if _, err := Handshake(testCtx(t), tr, mcp.Implementation{Name: "agenthub", Version: "test"}); err != nil {
		t.Fatalf("Handshake: %v", err)
	}

	p := <-listens
	if p.Meta == nil || p.Meta.ProtocolVersion != mcp.Version2026 {
		t.Fatalf("listen _meta = %+v", p.Meta)
	}
	if !p.Notifications.ToolsListChanged || !p.Notifications.PromptsListChanged ||
		!p.Notifications.ResourcesListChanged {
		t.Fatalf("listen filter %+v does not opt into all three list-changed types", p.Notifications)
	}
	if p.Notifications.ResourceSubscriptions != nil {
		t.Fatalf("listen filter subscribed to resources %v; this client subscribes to none",
			p.Notifications.ResourceSubscriptions)
	}

	select {
	case m := <-changed:
		if m != ChangeTools {
			t.Fatalf("mask = %s", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("subscriptions/listen stream delivered nothing")
	}
	// The broken stream is re-established with a fresh POST.
	select {
	case <-listens:
	case <-time.After(5 * time.Second):
		t.Fatal("subscriptions/listen never reconnected")
	}
}

// TestStreamableHTTPNotificationStreamGivesUpOn405 pins that a server which
// does not offer the GET stream is not hammered.
func TestStreamableHTTPNotificationStreamGivesUpOn405(t *testing.T) {
	var gets int
	done := make(chan struct{})
	fs := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodGet {
			gets++
			if gets == 1 {
				close(done)
			}
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		req := readRequestRPC(t, r)
		writeJSONRPC(t, w, mcp.NewResponse(req.ID, initResult(t, mcp.ProtocolVersion)))
	})
	tr := dialStreamable(t, HTTPConfig{
		URL:                fs.URL + "/mcp",
		NotificationStream: true,
		retryBase:          time.Millisecond,
	})
	if _, err := tr.Call(testCtx(t), mcp.MethodInitialize, mcp.InitializeParams{}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	<-done
	time.Sleep(50 * time.Millisecond)
	if gets != 1 {
		t.Fatalf("GET attempted %d times after 405, want exactly 1", gets)
	}
}

// TestStreamableHTTPResume covers best-effort Last-Event-ID resumption of a
// POST stream that died before delivering its response.
func TestStreamableHTTPResume(t *testing.T) {
	var pendingID mcp.ID
	fs := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if got := r.Header.Get(headerLastEventID); got != "ev-1" {
				t.Errorf("resume Last-Event-ID = %q, want ev-1", got)
			}
			s := startSSE(t, w)
			s.message("ev-2", mcp.NewResponse(pendingID, json.RawMessage(`{"resumed":true}`)))
			return
		}
		req := readRequestRPC(t, r)
		pendingID = req.ID
		s := startSSE(t, w)
		s.message("ev-1", mcp.NewNotification(mcp.NotificationPromptsListChanged, nil))
		// Handler returns: the stream dies before the response.
	})

	tr := dialStreamable(t, HTTPConfig{URL: fs.URL + "/mcp"})
	raw, err := tr.Call(testCtx(t), mcp.MethodToolsCall, nil)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if string(raw) != `{"resumed":true}` {
		t.Fatalf("result = %s", raw)
	}
}

// TestStreamableHTTPResumeRespectsRetryHint: over real wire bytes, the
// resume waits out the `retry` the server asked for.
//
// MCP 2025-11-25 words this as a MUST, and this is the path it binds: the
// per-POST stream is the only one the shipped product reconnects, and it
// used to come back in under a millisecond however long the server asked for
// — while the scanner's comment claimed a bounded backoff that belongs to a
// different loop. The gap between the two GETs is the whole assertion.
func TestStreamableHTTPResumeRespectsRetryHint(t *testing.T) {
	const hint = 250 * time.Millisecond
	var pendingID mcp.ID
	var broke time.Time
	resumed := make(chan time.Duration, 1)
	fs := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			resumed <- time.Since(broke)
			s := startSSE(t, w)
			s.message("ev-2", mcp.NewResponse(pendingID, json.RawMessage(`{"resumed":true}`)))
			return
		}
		req := readRequestRPC(t, r)
		pendingID = req.ID
		s := startSSE(t, w)
		s.message("ev-1", mcp.NewNotification(mcp.NotificationPromptsListChanged, nil))
		s.retry(int(hint.Milliseconds()))
		broke = time.Now()
		// Handler returns: the connection dies, the stream did not end.
	})

	tr := dialStreamable(t, HTTPConfig{URL: fs.URL + "/mcp"})
	raw, err := tr.Call(testCtx(t), mcp.MethodToolsCall, nil)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if string(raw) != `{"resumed":true}` {
		t.Fatalf("result = %s", raw)
	}
	gap := <-resumed
	// A margin below the ask, not above: the timer's own granularity is the
	// only thing being tolerated, and the failure this guards is a resume
	// that took microseconds.
	if gap < hint-20*time.Millisecond {
		t.Fatalf("resumed after %v, sooner than the %v the server asked for", gap, hint)
	}
}

// TestStreamableHTTPResumeSkippedWhenTheHintOutlastsTheCall: the other
// direction of the same rule. A wait that does not fit the caller's deadline
// cannot be taken, and the answer is to abandon the resume — not to come
// back early, which is the one thing the MUST forbids. The call then reports
// the stream break it already had.
func TestStreamableHTTPResumeSkippedWhenTheHintOutlastsTheCall(t *testing.T) {
	var gets atomic.Int32
	fs := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			gets.Add(1)
			w.WriteHeader(http.StatusOK)
			return
		}
		_ = readRequestRPC(t, r)
		s := startSSE(t, w)
		s.message("ev-1", mcp.NewNotification(mcp.NotificationPromptsListChanged, nil))
		s.retry(600_000) // ten minutes, far past any call deadline
	})

	tr := dialStreamable(t, HTTPConfig{URL: fs.URL + "/mcp"})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	_, err := tr.Call(ctx, mcp.MethodToolsCall, nil)
	if err == nil {
		t.Fatal("call succeeded although the stream broke and the resume was not allowed")
	}
	if !strings.Contains(err.Error(), "stream ended before the response") {
		t.Fatalf("err = %v, want the stream-break failure", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %v — it slept into the deadline instead of declining at once", d)
	}
	if n := gets.Load(); n != 0 {
		t.Fatalf("made %d resume attempts, want none", n)
	}
}

func TestStreamableHTTPResumeDisabled(t *testing.T) {
	fs := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			t.Error("resumption attempted although DisableResume is set")
			w.WriteHeader(http.StatusOK)
			return
		}
		_ = readRequestRPC(t, r)
		s := startSSE(t, w)
		s.message("ev-1", mcp.NewNotification(mcp.NotificationPromptsListChanged, nil))
	})
	tr := dialStreamable(t, HTTPConfig{URL: fs.URL + "/mcp", DisableResume: true})
	_, err := tr.Call(testCtx(t), mcp.MethodToolsCall, nil)
	te := transportError(t, err)
	if te.Class != ClassUnavailable {
		t.Fatalf("class = %s, want unavailable", te.Class)
	}
	if !strings.Contains(err.Error(), "stream ended before the response") {
		t.Fatalf("err = %v", err)
	}
}

// TestStreamableHTTPCloseDeletesSession pins the session teardown and the
// post-Close contract.
func TestStreamableHTTPCloseDeletesSession(t *testing.T) {
	deleted := make(chan string, 1)
	fs := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleted <- r.Header.Get(headerSessionID)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		req := readRequestRPC(t, r)
		w.Header().Set(headerSessionID, "sid-7")
		writeJSONRPC(t, w, mcp.NewResponse(req.ID, initResult(t, mcp.ProtocolVersion)))
	})
	tr := dialStreamable(t, HTTPConfig{URL: fs.URL + "/mcp"})
	if _, err := tr.Call(testCtx(t), mcp.MethodInitialize, mcp.InitializeParams{}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case sid := <-deleted:
		if sid != "sid-7" {
			t.Fatalf("DELETE session id = %q", sid)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not DELETE the session")
	}
	// Close is idempotent and post-Close use is a typed failure.
	if err := tr.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	_, err := tr.Call(testCtx(t), mcp.MethodToolsList, nil)
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
	if err := tr.Notify(testCtx(t), mcp.NotificationInitialized, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("Notify err = %v, want ErrClosed", err)
	}
	if tr.Stderr() != "" {
		t.Fatalf("Stderr = %q, want empty for a non-stdio transport", tr.Stderr())
	}
}

// TestStreamableHTTPProtocolViolations pins the two shapes a server must
// not answer a request with.
func TestStreamableHTTPProtocolViolations(t *testing.T) {
	t.Run("202 with no body", func(t *testing.T) {
		fs := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusAccepted)
		})
		tr := dialStreamable(t, HTTPConfig{URL: fs.URL + "/mcp"})
		_, err := tr.Call(testCtx(t), mcp.MethodToolsList, nil)
		if !errors.Is(err, ErrHTTPProtocol) {
			t.Fatalf("err = %v, want ErrHTTPProtocol", err)
		}
	})
	t.Run("json body without our response", func(t *testing.T) {
		fs := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSONRPC(t, w, mcp.NewResponse(mcp.NewIntID(4242), json.RawMessage(`{}`)))
		})
		tr := dialStreamable(t, HTTPConfig{URL: fs.URL + "/mcp"})
		_, err := tr.Call(testCtx(t), mcp.MethodToolsList, nil)
		if !errors.Is(err, ErrHTTPProtocol) {
			t.Fatalf("err = %v, want ErrHTTPProtocol", err)
		}
	})
	t.Run("malformed sse payload", func(t *testing.T) {
		fs := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request) {
			s := startSSE(t, w)
			s.event("message", "e1", []byte(`{"jsonrpc":"1.0"}`))
		})
		tr := dialStreamable(t, HTTPConfig{URL: fs.URL + "/mcp"})
		_, err := tr.Call(testCtx(t), mcp.MethodToolsList, nil)
		if !errors.Is(err, mcp.ErrMalformedFrame) {
			t.Fatalf("err = %v, want ErrMalformedFrame", err)
		}
	})
}

// TestStreamableHTTPJSONRPCErrorIsFatal pins that an ordinary error
// response never trips the breaker.
func TestStreamableHTTPJSONRPCErrorIsFatal(t *testing.T) {
	fs := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		req := readRequestRPC(t, r)
		writeJSONRPC(t, w, mcp.NewErrorResponse(req.ID, &mcp.Error{Code: mcp.CodeInvalidParams, Message: "bad args"}))
	})
	tr := dialStreamable(t, HTTPConfig{URL: fs.URL + "/mcp"})
	_, err := tr.Call(testCtx(t), mcp.MethodToolsCall, nil)
	te := transportError(t, err)
	if te.Class != ClassFatal {
		t.Fatalf("class = %s, want fatal", te.Class)
	}
	var rpcErr *mcp.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != mcp.CodeInvalidParams {
		t.Fatalf("err = %v, want the peer's JSON-RPC error", err)
	}
}

// TestStreamableHTTPSpecErrorIsNotALegacyServer covers the HTTP half of the
// backward-compatibility rule. A 2026-07-28 server rejects a malformed probe
// with 400 and one of the codes the specification allocates for itself —
// exactly the status a pre-2026 server uses for an unknown pre-session POST.
// The status alone therefore cannot decide, and reading it as "legacy" sends
// initialize to the one server that does not implement it. The body's code
// is what tells them apart, so the transport carries it on Error.RPCCode and
// the probe surfaces the server's own error instead of falling back.
func TestStreamableHTTPSpecErrorIsNotALegacyServer(t *testing.T) {
	for _, tc := range []struct {
		name         string
		body         string
		wantFallback bool
	}{
		{
			name: "spec error keeps the modern server",
			body: `{"jsonrpc":"2.0","id":1,"error":{"code":-32020,` +
				`"message":"MCP-Protocol-Version header does not match _meta"}}`,
		},
		{
			// A legacy streamable-http server rejects an unknown pre-session
			// POST with a bare 400 and no JSON-RPC body at all.
			name: "opaque 400 still falls back", body: "no session", wantFallback: true,
		},
		{
			// An SDK's own -32000..-32019 code is grandfathered and says
			// nothing about the generation; it must not block the fallback.
			name: "legacy sdk code still falls back", wantFallback: true,
			body: `{"jsonrpc":"2.0","id":1,"error":{"code":-32001,"message":"sdk failure"}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sawInitialize := make(chan struct{}, 1)
			fs := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
				m := readRequestRPC(t, r)
				if m.Method == mcp.MethodInitialize {
					select {
					case sawInitialize <- struct{}{}:
					default:
					}
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, tc.body)
			})
			tr := dialStreamable(t, HTTPConfig{URL: fs.URL})

			_, err := Handshake(testCtx(t), tr, mcp.Implementation{Name: "agenthub", Version: "test"})
			if err == nil {
				t.Fatal("handshake succeeded against a server that rejected everything")
			}
			var fellBack bool
			select {
			case <-sawInitialize:
				fellBack = true
			default:
			}
			if fellBack != tc.wantFallback {
				t.Fatalf("fell back to initialize = %v, want %v (err: %v)", fellBack, tc.wantFallback, err)
			}
		})
	}
}

// TestStreamRefusedPermanently: which statuses end the out-of-call stream
// loop. The whole 4xx class does, because the request never changes — the
// loop resends the identical GET (or subscriptions/listen POST) every time,
// so a server that refused it once refuses it forever and the retry cannot
// terminate. Before this the list was six named statuses and 400 was not
// among them, which is the status a server that does not understand the GET
// is most likely to answer: one request every five seconds, per downstream,
// for the life of the connection.
func TestStreamRefusedPermanently(t *testing.T) {
	for _, tc := range []struct {
		code int
		want bool
	}{
		{http.StatusBadRequest, true}, // the one the named list missed
		{http.StatusUnauthorized, true},
		{http.StatusForbidden, true},
		{http.StatusNotFound, true},
		{http.StatusMethodNotAllowed, true},
		{http.StatusGone, true},
		{http.StatusUnsupportedMediaType, true},
		{http.StatusNotImplemented, true}, // about the endpoint, not its health
		// "Later", which is what a retry is for.
		{http.StatusRequestTimeout, false},
		{http.StatusTooManyRequests, false},
		// Health, not the request.
		{http.StatusInternalServerError, false},
		{http.StatusBadGateway, false},
		{http.StatusServiceUnavailable, false},
		{http.StatusGatewayTimeout, false},
	} {
		if got := streamRefusedPermanently(tc.code); got != tc.want {
			t.Errorf("streamRefusedPermanently(%d) = %v, want %v", tc.code, got, tc.want)
		}
	}
}

// A 400 must end the loop rather than become a five-second heartbeat. This
// drives the real loop rather than the predicate, because the predicate
// being right is not the same as the loop reading it.
func TestNotificationStreamGivesUpOnBadRequest(t *testing.T) {
	var gets int
	done := make(chan struct{})
	fs := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodGet {
			gets++
			if gets == 1 {
				close(done)
			}
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		req := readRequestRPC(t, r)
		writeJSONRPC(t, w, mcp.NewResponse(req.ID, initResult(t, mcp.ProtocolVersion)))
	})
	tr := dialStreamable(t, HTTPConfig{
		URL:                fs.URL + "/mcp",
		NotificationStream: true,
		retryBase:          time.Millisecond,
	})
	if _, err := tr.Call(testCtx(t), mcp.MethodInitialize, mcp.InitializeParams{}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	<-done
	time.Sleep(80 * time.Millisecond)
	if gets != 1 {
		t.Fatalf("GET attempted %d times after a 400, want exactly 1", gets)
	}
}
