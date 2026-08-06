package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// --- shared harness -------------------------------------------------------

// recordedRequest is one request as it appeared on the wire. Volatile
// headers (Host, User-Agent, Accept-Encoding, Content-Length) are dropped so
// golden files pin only what this package controls.
type recordedRequest struct {
	Method string            `json:"method"`
	Path   string            `json:"path"`
	Header map[string]string `json:"header"`
	Body   json.RawMessage   `json:"body,omitempty"`
}

var volatileHeaders = map[string]bool{
	"Host": true, "User-Agent": true, "Accept-Encoding": true,
	"Content-Length": true, "Connection": true,
}

// fakeServer is an httptest.Server that records every request it saw.
type fakeServer struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []recordedRequest
}

func newFakeServer(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) *fakeServer {
	t.Helper()
	fs := &fakeServer{}
	fs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		rec := recordedRequest{
			Method: r.Method,
			Path:   r.URL.RequestURI(),
			Header: map[string]string{},
		}
		for k, v := range r.Header {
			if volatileHeaders[k] {
				continue
			}
			rec.Header[k] = strings.Join(v, ", ")
		}
		if len(body) > 0 && json.Valid(body) {
			rec.Body = json.RawMessage(body)
		}
		fs.mu.Lock()
		fs.reqs = append(fs.reqs, rec)
		fs.mu.Unlock()

		r.Body = io.NopCloser(strings.NewReader(string(body)))
		h(w, r)
	}))
	t.Cleanup(fs.Close)
	return fs
}

func (fs *fakeServer) recorded() []recordedRequest {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	out := make([]recordedRequest, len(fs.reqs))
	copy(out, fs.reqs)
	return out
}

func (fs *fakeServer) count() int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return len(fs.reqs)
}

// readRPC decodes the JSON-RPC message a handler received.
func readRPC(t *testing.T, r *http.Request) any {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	msg, err := mcp.ParseMessage(body)
	if err != nil {
		t.Fatalf("parse request body %q: %v", body, err)
	}
	return msg
}

func readRequestRPC(t *testing.T, r *http.Request) *mcp.Request {
	t.Helper()
	msg := readRPC(t, r)
	req, ok := msg.(*mcp.Request)
	if !ok {
		t.Fatalf("body is %T, want *mcp.Request", msg)
	}
	return req
}

// writeJSONRPC answers with a single application/json message.
func writeJSONRPC(t *testing.T, w http.ResponseWriter, msg any) {
	t.Helper()
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// sseWriter writes events on an open text/event-stream response.
type sseWriter struct {
	t *testing.T
	w http.ResponseWriter
	f http.Flusher
}

func startSSE(t *testing.T, w http.ResponseWriter) *sseWriter {
	t.Helper()
	f, ok := w.(http.Flusher)
	if !ok {
		t.Fatal("ResponseWriter is not a Flusher")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	f.Flush()
	return &sseWriter{t: t, w: w, f: f}
}

func (s *sseWriter) event(name, id string, data []byte) {
	s.t.Helper()
	var b strings.Builder
	if id != "" {
		fmt.Fprintf(&b, "id: %s\n", id)
	}
	if name != "" {
		fmt.Fprintf(&b, "event: %s\n", name)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fmt.Fprintf(&b, "data: %s\n", line)
	}
	b.WriteString("\n")
	if _, err := io.WriteString(s.w, b.String()); err != nil {
		return // client hung up; nothing to assert here
	}
	s.f.Flush()
}

func (s *sseWriter) message(id string, msg any) {
	s.t.Helper()
	data, err := json.Marshal(msg)
	if err != nil {
		s.t.Fatalf("marshal: %v", err)
	}
	s.event("message", id, data)
}

// idOnly writes an event carrying nothing but an id — the shape a resumable
// stream uses to advance Last-Event-ID without sending a message. The SSE
// dispatch rule drops it for having an empty data buffer; sseScanner
// dispatches it, and the consumers skip it (see the sseScanner doc).
func (s *sseWriter) idOnly(id string) {
	_, _ = fmt.Fprintf(s.w, "id: %s\n\n", id)
	s.f.Flush()
}

// retry writes a bare `retry:` field — what a 2025-11-25 server SHOULD send
// before closing a connection it does not mean to end the stream on.
func (s *sseWriter) retry(ms int) {
	_, _ = fmt.Fprintf(s.w, "retry: %d\n\n", ms)
	s.f.Flush()
}

func (s *sseWriter) comment(text string) {
	_, _ = fmt.Fprintf(s.w, ": %s\n\n", text)
	s.f.Flush()
}

// transportError asserts err is a *Error and returns it.
func transportError(t *testing.T, err error) *Error {
	t.Helper()
	if err == nil {
		t.Fatal("want error, got nil")
	}
	var te *Error
	if !errors.As(err, &te) {
		t.Fatalf("error %v (%T) is not *transport.Error", err, err)
	}
	return te
}

// --- unit tests -----------------------------------------------------------

func TestHTTPConfigValidation(t *testing.T) {
	tests := []struct {
		name string
		cfg  HTTPConfig
		want string
	}{
		{
			name: "client and dialer are mutually exclusive",
			cfg: HTTPConfig{
				URL:         "http://127.0.0.1:1/mcp",
				Client:      &http.Client{},
				DialContext: func(context.Context, string, string) (net.Conn, error) { return nil, nil },
			},
			want: "mutually exclusive",
		},
		{name: "bad scheme", cfg: HTTPConfig{URL: "ftp://example.com/mcp"}, want: "scheme"},
		{name: "no host", cfg: HTTPConfig{URL: "http:///mcp"}, want: "no host"},
		{name: "unparsable", cfg: HTTPConfig{URL: "http://[::1"}, want: "parse url"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := DialStreamableHTTP(tt.cfg); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want mention of %q", err, tt.want)
			} else if te := transportError(t, err); te.Class != ClassFatal {
				t.Fatalf("class = %s, want fatal (config errors are not retryable)", te.Class)
			}
			if _, err := DialHTTPSSE(testCtx(t), tt.cfg); err == nil {
				t.Fatal("legacy transport accepted an invalid config")
			}
		})
	}
}

func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"2", 2 * time.Second},
		{"0", 0},
		{"-5", 0},
		{"garbage", 0},
		{time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat), 0},
	}
	for _, tt := range tests {
		if got := parseRetryAfter(tt.in); got != tt.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
	// An HTTP-date in the future yields a positive, bounded delay.
	future := time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
	if d := parseRetryAfter(future); d <= 0 || d > 31*time.Second {
		t.Errorf("parseRetryAfter(future) = %v", d)
	}
}

// TestDialFailureIsRetryable pins the one case where a non-idempotent
// tools/call may safely be retried: nothing ever reached the server.
func TestDialFailureIsRetryable(t *testing.T) {
	// A port nothing listens on: the dial fails, the request is never sent.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	tr, err := DialStreamableHTTP(HTTPConfig{URL: "http://" + addr + "/mcp"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = tr.Close() }()

	_, err = tr.Call(testCtx(t), mcp.MethodPing, nil)
	te := transportError(t, err)
	if te.Class != ClassRetry {
		t.Fatalf("class = %s, want retry (the request never reached a server)", te.Class)
	}
}

// TestDialContextHookIsUsed proves the SSRF seam: internal/mcp never
// imports internal/guard, the caller injects screening as a dialer.
func TestDialContextHookIsUsed(t *testing.T) {
	blocked := errors.New("blocked by test netguard")
	var (
		mu    sync.Mutex
		calls int
	)

	tr, err := DialStreamableHTTP(HTTPConfig{
		URL: "http://192.0.2.1:9/mcp",
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			return nil, blocked
		},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = tr.Close() }()

	_, err = tr.Call(testCtx(t), mcp.MethodPing, nil)
	if err == nil || !strings.Contains(err.Error(), "blocked by test netguard") {
		t.Fatalf("err = %v, want the injected dialer's rejection", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls == 0 {
		t.Fatal("injected DialContext was never called")
	}
}

func TestDecodeMessagesAcceptsLegacyBatch(t *testing.T) {
	body := []byte(`[{"jsonrpc":"2.0","id":1,"result":{"ok":true}},{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}]`)
	msgs, err := decodeMessages(body)
	if err != nil {
		t.Fatalf("decodeMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("len = %d, want 2", len(msgs))
	}
	if _, ok := msgs[0].(*mcp.Response); !ok {
		t.Fatalf("msgs[0] = %T", msgs[0])
	}
	if _, ok := msgs[1].(*mcp.Notification); !ok {
		t.Fatalf("msgs[1] = %T", msgs[1])
	}
	if _, err := decodeMessages([]byte("   ")); !errors.Is(err, mcp.ErrMalformedFrame) {
		t.Fatalf("empty body err = %v, want ErrMalformedFrame", err)
	}
}

func TestSameOrigin(t *testing.T) {
	base := mustURL(t, "https://Example.com:8443/mcp")
	if !sameOrigin(base, mustURL(t, "https://example.com:8443/messages")) {
		t.Error("case-insensitive host/scheme must match")
	}
	if sameOrigin(base, mustURL(t, "https://evil.com:8443/messages")) {
		t.Error("different host must not match")
	}
	if sameOrigin(base, mustURL(t, "https://example.com/messages")) {
		t.Error("different port must not match")
	}
	if sameOrigin(base, mustURL(t, "http://example.com:8443/messages")) {
		t.Error("different scheme must not match")
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

// TestDrainSnippetFlattens pins that error snippets can never forge a log
// record: newlines become spaces and control bytes are dropped.
func TestDrainSnippetFlattens(t *testing.T) {
	fs := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "boom\nlevel=info msg=\"forged\"\x00\x07")
	})
	tr, err := DialStreamableHTTP(HTTPConfig{URL: fs.URL})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = tr.Close() }()

	_, err = tr.Call(testCtx(t), mcp.MethodPing, nil)
	if err == nil {
		t.Fatal("want error")
	}
	if strings.ContainsAny(err.Error(), "\n\x00\x07") {
		t.Fatalf("error string carries raw control bytes: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("snippet lost: %q", err.Error())
	}
}

// TestHTTPErrorCarriesItsStatus pins the field callers classify on. Before it
// existed the only route was grepping Error() for "http 401" — and that string
// includes the response-BODY snippet, so the match answered a question nobody
// asked.
func TestHTTPErrorCarriesItsStatus(t *testing.T) {
	for _, code := range []int{400, 401, 403, 404, 410, 429, 500, 502, 503} {
		err := httpError(fakeResponse(t, code, "body"))
		if err.StatusCode != code {
			t.Errorf("status %d produced StatusCode %d", code, err.StatusCode)
		}
		if got := StatusOf(err); got != code {
			t.Errorf("StatusOf for %d = %d", code, got)
		}
		// Wrapped in layers of context, exactly as a caller receives it.
		wrapped := fmt.Errorf("downstream %q: dial: %w", "srv", err)
		if got := StatusOf(wrapped); got != code {
			t.Errorf("StatusOf through a wrap for %d = %d", code, got)
		}
	}

	// A non-HTTP error has no status rather than a misleading zero-valued one.
	if got := StatusOf(errors.New("dial tcp: connection refused")); got != 0 {
		t.Errorf("StatusOf of a non-transport error = %d, want 0", got)
	}
	if got := StatusOf(nil); got != 0 {
		t.Errorf("StatusOf(nil) = %d, want 0", got)
	}
}

// TestIsAuthStatusIgnoresTheResponseBody is the regression this field was
// added for.
//
// A proxy in front of an MCP server answers 502 and explains itself in the
// body: "upstream returned http 401". The body is folded into the transport
// error's message, so classifying by substring reported the operator's
// credentials as rejected and sent them to run `auth login` — for a failure no
// credential can fix, at a hop they cannot see.
func TestIsAuthStatusIgnoresTheResponseBody(t *testing.T) {
	proxy := httpError(fakeResponse(t, 502, "upstream returned http 401 for this request"))
	if IsAuthStatus(proxy) {
		t.Errorf("a 502 was classified as an auth rejection: %v", proxy)
	}
	// The substring approach really would have been fooled — otherwise this
	// test proves nothing about the change.
	if !strings.Contains(proxy.Error(), "http 401") {
		t.Fatalf("the body snippet is no longer in the message, so this case no longer models the bug: %v", proxy)
	}

	for _, code := range []int{401, 403} {
		if !IsAuthStatus(httpError(fakeResponse(t, code, ""))) {
			t.Errorf("status %d was not classified as an auth rejection", code)
		}
	}
	for _, code := range []int{400, 404, 429, 500, 502} {
		if IsAuthStatus(httpError(fakeResponse(t, code, ""))) {
			t.Errorf("status %d was classified as an auth rejection", code)
		}
	}
	if IsAuthStatus(errors.New("connection refused")) {
		t.Error("a non-HTTP error was classified as an auth rejection")
	}
}

// fakeResponse builds a *http.Response the way httpError expects to receive
// one (it reads Request for the "where" prefix and Body for the snippet).
func fakeResponse(t *testing.T, code int, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://srv.example/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Response{
		StatusCode: code,
		Request:    req,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{},
	}
}

// TestNotFoundWithARPCBodyIsNotASessionExpiry: MCP 2026-07-28 makes 404 the
// required answer for an unimplemented METHOD, so the status alone stopped
// being enough to mean "your session is gone" — and on a 2026 connection
// there is no session to have expired, so that reading named a cause that
// could not be true. The JSON-RPC body is what tells them apart, and
// httpError already had the code parsed and unused.
func TestNotFoundWithARPCBodyIsNotASessionExpiry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		body      string
		wantErr   error
		wantClass Class
	}{
		{
			name:    "method not found",
			body:    `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"no such method"}}`,
			wantErr: ErrMethodNotFound,
			// Fatal: retrying cannot make the server implement it, and
			// ClassUnavailable exists to make the caller re-initialize.
			wantClass: ClassFatal,
		},
		{
			// A legacy server dropping a session answers a bare 404.
			name: "no body", body: "",
			wantErr: ErrSessionExpired, wantClass: ClassUnavailable,
		},
		{
			name: "body that is not JSON-RPC", body: "gone",
			wantErr: ErrSessionExpired, wantClass: ClassUnavailable,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fs := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, tc.body)
			})
			tr := dialStreamable(t, HTTPConfig{URL: fs.URL})
			tr.sessionID = "sess-1"

			_, err := tr.Call(testCtx(t), mcp.MethodToolsList, nil)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			var te *Error
			if !errors.As(err, &te) || te.Class != tc.wantClass {
				t.Fatalf("class = %v, want %v", te, tc.wantClass)
			}
			// A 404 that named a method must not throw away a live session:
			// "I do not serve that" is not "reconnect".
			tr.mu.Lock()
			sid := tr.sessionID
			tr.mu.Unlock()
			if tc.wantErr == ErrMethodNotFound && sid == "" {
				t.Fatal("a method-not-found 404 cleared the session id")
			}
			if tc.wantErr == ErrSessionExpired && sid != "" {
				t.Fatalf("a session-expiry 404 left the session id %q", sid)
			}
		})
	}
}
